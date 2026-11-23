// Package usage reads token usage and its cost from sub2api's usage_logs and
// exposes per-account rolling windows and per-provider hourly and daily
// buckets.
//
// Usage is kept in memory as hourly buckets per account. A full sync reads the
// whole span once (and again every resync interval); in between only the last
// hour or two are read again and replace what was there.
package usage

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KexiChanProjectAI/llm-usage-exporter/internal/config"
)

// Querier is satisfied by *pgxpool.Pool, *pgx.Conn and pgx.Tx.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Agg is usage summed over a set of usage_logs rows. Costs are in USD, as
// sub2api stores them.
type Agg struct {
	Requests      float64
	Input         float64
	Output        float64
	CacheCreation float64
	CacheRead     float64
	// TotalCost is the list-price value of the tokens (usage_logs.total_cost).
	TotalCost float64
	// ActualCost is what sub2api billed its users (actual_cost, after the
	// group rate multiplier).
	ActualCost float64
	// AccountCost is the cost on the account side, computed as sub2api's
	// dashboard does: COALESCE(account_stats_cost, total_cost) *
	// COALESCE(account_rate_multiplier, 1).
	AccountCost float64
}

func (a *Agg) add(b *Agg) {
	a.Requests += b.Requests
	a.Input += b.Input
	a.Output += b.Output
	a.CacheCreation += b.CacheCreation
	a.CacheRead += b.CacheRead
	a.TotalCost += b.TotalCost
	a.ActualCost += b.ActualCost
	a.AccountCost += b.AccountCost
}

// Tokens returns the token counts by type, in a fixed order.
func (a *Agg) Tokens() [4]struct {
	Type  string
	Value float64
} {
	return [4]struct {
		Type  string
		Value float64
	}{{"input", a.Input}, {"output", a.Output}, {"cache_creation", a.CacheCreation}, {"cache_read", a.CacheRead}}
}

// Costs returns the costs by kind, in a fixed order.
func (a *Agg) Costs() [3]struct {
	Kind  string
	Value float64
} {
	return [3]struct {
		Kind  string
		Value float64
	}{{"total", a.TotalCost}, {"actual", a.ActualCost}, {"account", a.AccountCost}}
}

// Row is one account's usage in one hour.
type Row struct {
	AccountID int64
	Hour      time.Time
	Agg
}

// AccountMeta is the part of an accounts row the usage metrics label with.
type AccountMeta struct {
	Name     string
	Platform string
}

// The hour is truncated in the configured time zone, so that hours line up
// with local days even where the offset is not a whole number of hours.
const usageQuery = `
SELECT account_id,
       date_trunc('hour', created_at, $2) AS hour,
       count(*)::float8,
       COALESCE(sum(input_tokens), 0)::float8,
       COALESCE(sum(output_tokens), 0)::float8,
       COALESCE(sum(cache_creation_tokens), 0)::float8,
       COALESCE(sum(cache_read_tokens), 0)::float8,
       COALESCE(sum(total_cost), 0)::float8,
       COALESCE(sum(actual_cost), 0)::float8,
       COALESCE(sum(COALESCE(account_stats_cost, total_cost) * COALESCE(account_rate_multiplier, 1)), 0)::float8
FROM usage_logs
WHERE created_at >= $1
GROUP BY 1, 2`

// QueryHours returns usage per account and hour since the given time.
func QueryHours(ctx context.Context, db Querier, since time.Time, tz string) ([]Row, error) {
	rows, err := db.Query(ctx, usageQuery, since, tz)
	if err != nil {
		return nil, fmt.Errorf("query usage_logs: %w", err)
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		var r Row
		if err := rows.Scan(&r.AccountID, &r.Hour, &r.Requests, &r.Input, &r.Output, &r.CacheCreation, &r.CacheRead,
			&r.TotalCost, &r.ActualCost, &r.AccountCost); err != nil {
			return nil, fmt.Errorf("scan usage: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read usage: %w", err)
	}
	return out, nil
}

// Soft-deleted accounts are included: their past usage still counts.
const accountsQuery = `SELECT id, name, platform FROM accounts`

// QueryAccounts returns the name and platform of every account.
func QueryAccounts(ctx context.Context, db Querier) (map[int64]AccountMeta, error) {
	rows, err := db.Query(ctx, accountsQuery)
	if err != nil {
		return nil, fmt.Errorf("query accounts: %w", err)
	}
	defer rows.Close()
	out := map[int64]AccountMeta{}
	for rows.Next() {
		var (
			id int64
			m  AccountMeta
		)
		if err := rows.Scan(&id, &m.Name, &m.Platform); err != nil {
			return nil, fmt.Errorf("scan account: %w", err)
		}
		out[id] = m
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read accounts: %w", err)
	}
	return out, nil
}

// Source reads usage; QueryHours and QueryAccounts against a pool in
// production, fakes in tests.
type Source interface {
	Hours(ctx context.Context, since time.Time, tz string) ([]Row, error)
	Accounts(ctx context.Context) (map[int64]AccountMeta, error)
}

// DBSource reads from the sub2api database.
type DBSource struct{ DB Querier }

func (s DBSource) Hours(ctx context.Context, since time.Time, tz string) ([]Row, error) {
	return QueryHours(ctx, s.DB, since, tz)
}

func (s DBSource) Accounts(ctx context.Context) (map[int64]AccountMeta, error) {
	return QueryAccounts(ctx, s.DB)
}

// ProviderOf returns, per account_id label, the provider the quota collector
// classified the account as. Accounts it does not track fall back to their
// sub2api platform.
type ProviderOf func() map[string]string

// Collector keeps the usage cache and exports it.
type Collector struct {
	cfg        config.Usage
	loc        *time.Location
	src        Source
	providerOf ProviderOf
	log        *slog.Logger
	now        func() time.Time

	mu          sync.Mutex
	hours       map[int64]map[int64]*Agg // account -> hour start (unix) -> usage
	accounts    map[int64]AccountMeta
	lastFull    time.Time
	up          bool
	attempted   bool
	lastSuccess time.Time
	lastDur     time.Duration
}

func New(cfg config.Usage, src Source, providerOf ProviderOf, log *slog.Logger) (*Collector, error) {
	loc, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		return nil, fmt.Errorf("usage.timezone: %w", err)
	}
	if providerOf == nil {
		providerOf = func() map[string]string { return nil }
	}
	return &Collector{
		cfg: cfg, loc: loc, src: src, providerOf: providerOf, log: log, now: time.Now,
		hours: map[int64]map[int64]*Agg{}, accounts: map[int64]AccountMeta{},
	}, nil
}

// Run refreshes the cache every interval until ctx is done.
func (c *Collector) Run(ctx context.Context) {
	c.Refresh(ctx)
	t := time.NewTicker(c.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.Refresh(ctx)
		}
	}
}

func (c *Collector) hourStart(t time.Time) time.Time {
	t = t.In(c.loc)
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, c.loc)
}

func (c *Collector) dayStart(t time.Time) time.Time {
	t = t.In(c.loc)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, c.loc)
}

// spanStart is the oldest hour any window or bucket needs.
func (c *Collector) spanStart(now time.Time) time.Time {
	h := c.hourStart(now)
	start := h.Add(-time.Duration(c.cfg.HourlyBuckets-1) * time.Hour)
	if d := c.dayStart(now).AddDate(0, 0, -(c.cfg.DailyBuckets - 1)); d.Before(start) {
		start = d
	}
	for _, days := range c.cfg.Windows {
		if w := h.Add(-time.Duration(days*24-1) * time.Hour); w.Before(start) {
			start = w
		}
	}
	return start
}

// Refresh reads new usage: everything in the span on the first run and every
// resync interval, otherwise only the last Lookback.
func (c *Collector) Refresh(ctx context.Context) {
	now := c.now()
	c.mu.Lock()
	full := c.lastFull.IsZero() || now.Sub(c.lastFull) >= c.cfg.ResyncInterval
	c.mu.Unlock()

	span := c.spanStart(now)
	since := span
	if !full {
		since = c.hourStart(now.Add(-c.cfg.Lookback))
		if since.Before(span) {
			since = span
		}
	}

	ctx, cancel := context.WithTimeout(ctx, c.cfg.QueryTimeout)
	defer cancel()
	start := time.Now()
	accounts, err := c.src.Accounts(ctx)
	var rows []Row
	if err == nil {
		rows, err = c.src.Hours(ctx, since, c.cfg.Timezone)
	}
	dur := time.Since(start)

	c.mu.Lock()
	defer c.mu.Unlock()
	c.attempted = true
	c.lastDur = dur
	if err != nil {
		c.up = false
		if ctx.Err() == nil || full {
			c.log.Error("read usage from sub2api failed; keeping the previous values (the role needs SELECT on usage_logs)",
				"full", full, "error", err)
		}
		return
	}
	if full {
		c.hours = map[int64]map[int64]*Agg{}
		c.lastFull = now
	} else {
		cut := since.Unix()
		for _, hs := range c.hours {
			for h := range hs {
				if h >= cut {
					delete(hs, h)
				}
			}
		}
	}
	for i := range rows {
		r := &rows[i]
		hs := c.hours[r.AccountID]
		if hs == nil {
			hs = map[int64]*Agg{}
			c.hours[r.AccountID] = hs
		}
		// Hours are already truncated in SQL; normalise in case the database
		// returned another representation of the same instant.
		key := c.hourStart(r.Hour).Unix()
		if a := hs[key]; a != nil {
			a.add(&r.Agg)
		} else {
			agg := r.Agg
			hs[key] = &agg
		}
	}
	old := span.Unix()
	for id, hs := range c.hours {
		for h := range hs {
			if h < old {
				delete(hs, h)
			}
		}
		if len(hs) == 0 {
			delete(c.hours, id)
		}
	}
	c.accounts = accounts
	c.up = true
	c.lastSuccess = now
	c.log.Debug("read usage from sub2api", "full", full, "rows", len(rows), "duration", dur)
}

// Snapshot is the exported view of the cache at one instant.
type Snapshot struct {
	// Windows holds per-account totals over each configured window (days).
	Windows []AccountWindow
	// Buckets holds per-provider totals per hour and per day, zero-filled
	// for every provider that has usage anywhere in the buckets.
	Buckets []Bucket
}

type AccountWindow struct {
	Provider    string
	AccountID   string
	AccountName string
	Window      string
	Agg
}

type Bucket struct {
	Provider    string
	Granularity string // "hour" or "day"
	Label       string // bucket start in the configured zone
	Agg
}

// WindowLabel is the window label for a number of days.
func WindowLabel(days int) string { return strconv.Itoa(days) + "d" }

const (
	hourLabel = "2006-01-02 15:00"
	dayLabel  = "2006-01-02"
)

func (c *Collector) snapshot() Snapshot {
	now := c.now()
	providers := c.providerOf()
	c.mu.Lock()
	defer c.mu.Unlock()

	curHour := c.hourStart(now)
	hourFrom := curHour.Add(-time.Duration(c.cfg.HourlyBuckets-1) * time.Hour)
	today := c.dayStart(now)
	dayFrom := today.AddDate(0, 0, -(c.cfg.DailyBuckets - 1))

	type pkey struct{ provider, label string }
	hourly := map[pkey]*Agg{}
	daily := map[pkey]*Agg{}
	active := map[string]bool{}
	var snap Snapshot

	ids := make([]int64, 0, len(c.hours))
	for id := range c.hours {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		hs := c.hours[id]
		label := strconv.FormatInt(id, 10)
		meta := c.accounts[id]
		prov := providers[label]
		if prov == "" {
			prov = meta.Platform
		}
		if prov == "" {
			prov = "unknown"
		}
		name := meta.Name
		if name == "" {
			name = label
		}
		windows := make([]Agg, len(c.cfg.Windows))
		for h, a := range hs {
			t := time.Unix(h, 0).In(c.loc)
			for i, days := range c.cfg.Windows {
				if !t.Before(curHour.Add(-time.Duration(days*24-1) * time.Hour)) {
					windows[i].add(a)
				}
			}
			if !t.Before(hourFrom) {
				k := pkey{prov, t.Format(hourLabel)}
				if hourly[k] == nil {
					hourly[k] = &Agg{}
				}
				hourly[k].add(a)
				active[prov] = true
			}
			if d := c.dayStart(t); !d.Before(dayFrom) {
				k := pkey{prov, d.Format(dayLabel)}
				if daily[k] == nil {
					daily[k] = &Agg{}
				}
				daily[k].add(a)
				active[prov] = true
			}
		}
		for i, days := range c.cfg.Windows {
			snap.Windows = append(snap.Windows, AccountWindow{
				Provider: prov, AccountID: label, AccountName: name, Window: WindowLabel(days), Agg: windows[i]})
		}
	}

	provs := make([]string, 0, len(active))
	for p := range active {
		provs = append(provs, p)
	}
	sort.Strings(provs)
	for _, p := range provs {
		seen := map[string]bool{}
		for t := hourFrom; !t.After(curHour); t = t.Add(time.Hour) {
			b := Bucket{Provider: p, Granularity: "hour", Label: t.Format(hourLabel)}
			if seen[b.Label] { // the repeated hour when DST ends
				continue
			}
			seen[b.Label] = true
			if a := hourly[pkey{p, b.Label}]; a != nil {
				b.Agg = *a
			}
			snap.Buckets = append(snap.Buckets, b)
		}
		for d := dayFrom; !d.After(today); d = d.AddDate(0, 0, 1) {
			b := Bucket{Provider: p, Granularity: "day", Label: d.Format(dayLabel)}
			if a := daily[pkey{p, b.Label}]; a != nil {
				b.Agg = *a
			}
			snap.Buckets = append(snap.Buckets, b)
		}
	}
	return snap
}
