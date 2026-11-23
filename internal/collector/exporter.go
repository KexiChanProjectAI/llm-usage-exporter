// Package collector schedules quota fetches in the background and exposes the
// cached results as Prometheus metrics.
package collector

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/KexiChanProjectAI/llm-usage-exporter/internal/config"
	"github.com/KexiChanProjectAI/llm-usage-exporter/internal/httpclient"
	"github.com/KexiChanProjectAI/llm-usage-exporter/internal/provider"
	"github.com/KexiChanProjectAI/llm-usage-exporter/internal/source"
)

// Loader returns the current sub2api accounts.
type Loader func(ctx context.Context) ([]source.Account, error)

// Fetcher performs one quota fetch.
type Fetcher func(ctx context.Context, client *http.Client, t provider.Target) (*provider.Result, error)

// boundAccount is an account mapped to the group that fetches its quota.
type boundAccount struct {
	ID          string
	Name        string
	Platform    string
	Status      string
	Schedulable bool
	Provider    string
	GroupKey    string
}

// group is one distinct upstream fetch (target + proxy), shared by every
// account that resolves to it.
type group struct {
	key      string
	target   provider.Target
	keyHash  string
	proxyURL string
	interval time.Duration

	nextRun time.Time
	running bool

	attempted    bool
	lastAttempt  time.Time
	lastSuccess  time.Time
	lastDuration time.Duration
	up           bool
	authFailed   bool
	unsupported  bool
	lastStatus   int
	lastErr      string
	result       *provider.Result
}

type Exporter struct {
	cfg     *config.Config
	load    Loader
	fetch   Fetcher
	clients *httpclient.Pool
	log     *slog.Logger
	now     func() time.Time
	sem     chan struct{}
	wg      sync.WaitGroup

	mu         sync.Mutex
	accounts   []boundAccount
	groups     map[string]*group
	blacklist  map[string]time.Time // key hash -> until
	dbUp       bool
	dbReloaded time.Time
	stats      fetchStats
}

// fetchStats counts fetch outcomes per provider for llm_quota_fetch_total.
type fetchStats map[[2]string]float64

func New(cfg *config.Config, load Loader, fetch Fetcher, log *slog.Logger) *Exporter {
	if fetch == nil {
		fetch = provider.Fetch
	}
	return &Exporter{
		cfg:       cfg,
		load:      load,
		fetch:     fetch,
		clients:   httpclient.NewPool(cfg.Collect.Timeout),
		log:       log,
		now:       time.Now,
		sem:       make(chan struct{}, cfg.Collect.Concurrency),
		groups:    map[string]*group{},
		blacklist: map[string]time.Time{},
		stats:     fetchStats{},
	}
}

// Run reloads accounts and dispatches due fetches until ctx is done, then
// waits for in-flight fetches.
func (e *Exporter) Run(ctx context.Context) {
	e.reload(ctx)
	reload := time.NewTicker(e.cfg.Database.ReloadInterval)
	defer reload.Stop()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		e.dispatch(ctx)
		select {
		case <-ctx.Done():
			e.wg.Wait()
			return
		case <-reload.C:
			e.reload(ctx)
		case <-tick.C:
		}
	}
}

// resolveProxy applies the precedence: provider proxy > account proxy (when
// use_account_proxy) > global proxy > direct. "direct" at any level stops
// the chain.
func (e *Exporter) resolveProxy(providerID string, acc *source.Account) string {
	if p := e.cfg.Provider(providerID).Proxy; p != "" {
		if p == config.ProxyDirect {
			return ""
		}
		return p
	}
	if e.cfg.UseAccountProxyEnabled() && acc.ProxyURL != "" {
		return acc.ProxyURL
	}
	if e.cfg.Proxy != "" && e.cfg.Proxy != config.ProxyDirect {
		return e.cfg.Proxy
	}
	return ""
}

func (e *Exporter) reload(ctx context.Context) {
	loadCtx, cancel := context.WithTimeout(ctx, e.cfg.Database.QueryTimeout)
	defer cancel()
	accs, err := e.load(loadCtx)
	if err != nil {
		e.log.Error("load accounts from sub2api failed; keeping the previous account set", "error", err)
		e.mu.Lock()
		e.dbUp = false
		e.mu.Unlock()
		return
	}
	enabled := map[string]bool{}
	for _, id := range e.cfg.EnabledProviders() {
		enabled[id] = true
	}

	now := e.now()
	var bound []boundAccount
	type pending struct {
		target   provider.Target
		proxyURL string
	}
	wanted := map[string]pending{}
	for i := range accs {
		acc := &accs[i]
		t, ok := provider.Classify(acc)
		if !ok || !enabled[t.Provider] {
			continue
		}
		proxyURL := e.resolveProxy(t.Provider, acc)
		key := t.Identity() + "|" + proxyURL
		wanted[key] = pending{target: t, proxyURL: proxyURL}
		bound = append(bound, boundAccount{
			ID: acc.IDLabel(), Name: acc.Name, Platform: acc.Platform, Status: acc.Status,
			Schedulable: acc.Schedulable, Provider: t.Provider, GroupKey: key,
		})
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	keyHashes := map[string]bool{}
	proxies := map[string]bool{}
	for key, p := range wanted {
		interval := e.cfg.Provider(p.target.Provider).Interval
		if g, ok := e.groups[key]; ok {
			g.interval = interval
		} else {
			// Spread the first fetches of new groups over up to 30s.
			spread := min(interval, 30*time.Second)
			e.groups[key] = &group{
				key: key, target: p.target, keyHash: p.target.KeyHash(), proxyURL: p.proxyURL,
				interval: interval, nextRun: now.Add(time.Duration(rand.Int64N(int64(spread)))),
			}
		}
		keyHashes[p.target.KeyHash()] = true
		proxies[p.proxyURL] = true
	}
	for key, g := range e.groups {
		if _, ok := wanted[key]; !ok && !g.running {
			delete(e.groups, key)
		}
	}
	// A key that disappeared from sub2api (replaced or removed) drops its
	// blacklist entry; a re-added old key will be retried once.
	for h := range e.blacklist {
		if !keyHashes[h] {
			delete(e.blacklist, h)
		}
	}
	e.clients.Retain(proxies)
	e.accounts = bound
	e.dbUp = true
	e.dbReloaded = now
	e.log.Info("loaded accounts from sub2api", "candidates", len(accs), "quota_accounts", len(bound), "fetch_groups", len(wanted))
}

func (e *Exporter) dispatch(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	now := e.now()
	e.mu.Lock()
	var due []*group
	for _, g := range e.groups {
		if g.running || now.Before(g.nextRun) {
			continue
		}
		if until, ok := e.blacklist[g.keyHash]; ok {
			if now.Before(until) {
				g.nextRun = until
				continue
			}
			delete(e.blacklist, g.keyHash)
		}
		g.running = true
		due = append(due, g)
	}
	e.mu.Unlock()

	for _, g := range due {
		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			select {
			case e.sem <- struct{}{}:
			case <-ctx.Done():
				e.mu.Lock()
				g.running = false
				e.mu.Unlock()
				return
			}
			defer func() { <-e.sem }()
			e.runFetch(ctx, g)
		}()
	}
}

func (e *Exporter) runFetch(ctx context.Context, g *group) {
	start := e.now()
	var (
		res *provider.Result
		err error
	)
	client, cerr := e.clients.Get(g.proxyURL)
	if cerr != nil {
		err = cerr
	} else {
		fetchCtx, cancel := context.WithTimeout(ctx, 4*e.cfg.Collect.Timeout) // Command Code makes 4 calls
		res, err = e.fetch(fetchCtx, client, g.target)
		cancel()
	}
	end := e.now()

	e.mu.Lock()
	defer e.mu.Unlock()
	g.running = false
	g.attempted = true
	g.lastAttempt = end
	g.lastDuration = end.Sub(start)
	// ±10% jitter keeps groups from aligning over time.
	jitter := time.Duration((rand.Float64()*0.2 - 0.1) * float64(g.interval))
	g.nextRun = end.Add(g.interval + jitter)

	if err == nil {
		g.up, g.authFailed, g.unsupported, g.lastStatus, g.lastErr = true, false, false, http.StatusOK, ""
		g.result = res
		g.lastSuccess = end
		e.stats[[2]string{g.target.Provider, "success"}]++
		return
	}
	if ctx.Err() != nil {
		return // shutting down; not an upstream failure
	}

	status := provider.StatusOf(err)
	if errors.Is(err, provider.ErrUnsupported) {
		g.up, g.authFailed, g.unsupported = false, false, true
		g.lastStatus, g.lastErr, g.result = status, err.Error(), nil
		g.nextRun = end.Add(e.cfg.Collect.UnsupportedRetry)
		e.stats[[2]string{g.target.Provider, "unsupported"}]++
		e.log.Info("endpoint not offered by the account's host; retrying later",
			"provider", g.target.Provider, "accounts", e.accountIDs(g.key), "status", status,
			"retry_at", g.nextRun.UTC().Format(time.RFC3339), "error", g.lastErr)
		return
	}
	g.unsupported = false
	g.up = false
	g.lastStatus = status
	g.lastErr = err.Error()
	g.authFailed = status == http.StatusUnauthorized || status == http.StatusForbidden
	outcome := "error"
	switch {
	case g.authFailed:
		outcome = "auth_error"
	case status != 0:
		outcome = "http_error"
	case errors.Is(err, provider.ErrNoWindows):
		outcome = "no_windows"
	case errors.Is(err, provider.ErrInvalidResponse):
		outcome = "invalid_response"
	}
	e.stats[[2]string{g.target.Provider, outcome}]++

	if g.authFailed {
		// The key is rejected: its old quota no longer means anything.
		g.result = nil
	}
	attrs := []any{
		"provider", g.target.Provider, "accounts", e.accountIDs(g.key),
		"proxy", httpclient.Redact(g.proxyURL), "status", status, "error", g.lastErr,
	}
	if e.blacklistable(status) {
		until := end.Add(e.cfg.AuthBlacklist.Duration)
		e.blacklist[g.keyHash] = until
		g.nextRun = until
		e.log.Warn("quota fetch rejected the API key; skipping it until it changes or the blacklist expires",
			append(attrs, "blacklisted_until", until.UTC().Format(time.RFC3339))...)
		return
	}
	e.log.Warn("quota fetch failed", attrs...)
}

func (e *Exporter) blacklistable(status int) bool {
	if status == 0 {
		return false
	}
	for _, s := range e.cfg.AuthBlacklist.StatusCodes {
		if s == status {
			return true
		}
	}
	return false
}

// accountIDs lists the account IDs of a group, for logs. Caller holds e.mu.
func (e *Exporter) accountIDs(key string) []string {
	var ids []string
	for _, a := range e.accounts {
		if a.GroupKey == key {
			ids = append(ids, a.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

// Healthy reports whether the last account reload succeeded.
func (e *Exporter) Healthy() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.dbUp
}

// AccountProviders maps the account_id label of every tracked account to the
// provider it was classified as.
func (e *Exporter) AccountProviders() map[string]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make(map[string]string, len(e.accounts))
	for _, a := range e.accounts {
		out[a.ID] = a.Provider
	}
	return out
}
