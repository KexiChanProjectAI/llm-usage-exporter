package collector

import (
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

var base = []string{"provider", "account_id", "account_name"}

func withBase(extra ...string) []string {
	return append(append([]string{}, base...), extra...)
}

var (
	descUsedRatio = prometheus.NewDesc("llm_quota_used_ratio",
		"Used share of a rolling quota window (0-1, may exceed 1).", withBase("window"), nil)
	descReset = prometheus.NewDesc("llm_quota_reset_timestamp_seconds",
		"Unix time when the quota window resets.", withBase("window"), nil)
	descWindowUsed = prometheus.NewDesc("llm_quota_window_used",
		"Absolute amount used in the window, when the provider reports it.", withBase("window", "unit"), nil)
	descWindowLimit = prometheus.NewDesc("llm_quota_window_limit",
		"Absolute window limit, when the provider reports it.", withBase("window", "unit"), nil)
	descBalance = prometheus.NewDesc("llm_quota_balance",
		"Credit balance reported by the provider.", withBase("kind", "currency"), nil)
	descBalanceAvailable = prometheus.NewDesc("llm_quota_balance_available",
		"Provider's own flag that the balance is usable (DeepSeek is_available).", base, nil)
	descPlan = prometheus.NewDesc("llm_quota_plan_info",
		"Subscription plan reported by the provider.", withBase("plan"), nil)
	descUp = prometheus.NewDesc("llm_quota_up",
		"1 if the last quota fetch for the account succeeded.", base, nil)
	descAuthValid = prometheus.NewDesc("llm_quota_auth_valid",
		"0 if the last quota fetch was rejected with 401/403.", base, nil)
	descSupported = prometheus.NewDesc("llm_quota_endpoint_supported",
		"0 if the account's host does not offer the quota/balance endpoint (e.g. a relay).", base, nil)
	descBlacklisted = prometheus.NewDesc("llm_quota_key_blacklisted",
		"1 while the account's API key is skipped after an auth rejection.", base, nil)
	descBlacklistUntil = prometheus.NewDesc("llm_quota_key_blacklist_until_timestamp_seconds",
		"Unix time when the API key is retried (unless it changes first).", base, nil)
	descLastAttempt = prometheus.NewDesc("llm_quota_last_attempt_timestamp_seconds",
		"Unix time of the last quota fetch attempt.", base, nil)
	descLastSuccess = prometheus.NewDesc("llm_quota_last_success_timestamp_seconds",
		"Unix time of the last successful quota fetch.", base, nil)
	descDuration = prometheus.NewDesc("llm_quota_fetch_duration_seconds",
		"Duration of the last quota fetch.", base, nil)
	descHTTPStatus = prometheus.NewDesc("llm_quota_last_http_status",
		"HTTP status of the last quota fetch (0 = no response).", base, nil)
	descAccountInfo = prometheus.NewDesc("llm_quota_account_info",
		"sub2api account metadata.", withBase("platform", "status", "schedulable"), nil)
	descAccounts = prometheus.NewDesc("llm_exporter_accounts",
		"Accounts with a supported quota, per provider.", []string{"provider"}, nil)
	descFetchTotal = prometheus.NewDesc("llm_quota_fetch_total",
		"Quota fetches by outcome.", []string{"provider", "result"}, nil)
	descDBUp = prometheus.NewDesc("llm_exporter_db_up",
		"1 if the last sub2api account reload succeeded.", nil, nil)
	descDBReload = prometheus.NewDesc("llm_exporter_db_last_success_timestamp_seconds",
		"Unix time of the last successful sub2api account reload.", nil, nil)
)

func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{
		descUsedRatio, descReset, descWindowUsed, descWindowLimit, descBalance, descBalanceAvailable, descPlan,
		descUp, descAuthValid, descSupported, descBlacklisted, descBlacklistUntil, descLastAttempt,
		descLastSuccess, descDuration, descHTTPStatus, descAccountInfo, descAccounts,
		descFetchTotal, descDBUp, descDBReload,
	} {
		ch <- d
	}
}

func gauge(ch chan<- prometheus.Metric, d *prometheus.Desc, v float64, labels ...string) {
	ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, labels...)
}

func boolF(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func unixF(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }

func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	now := e.now()
	e.mu.Lock()
	defer e.mu.Unlock()

	gauge(ch, descDBUp, boolF(e.dbUp))
	if !e.dbReloaded.IsZero() {
		gauge(ch, descDBReload, unixF(e.dbReloaded))
	}
	for k, v := range e.stats {
		ch <- prometheus.MustNewConstMetric(descFetchTotal, prometheus.CounterValue, v, k[0], k[1])
	}

	perProvider := map[string]float64{}
	for _, id := range e.cfg.EnabledProviders() {
		perProvider[id] = 0
	}
	// Two accounts may share a name; account_id keeps their series apart, but
	// the exact same account must not be emitted twice.
	seen := map[string]bool{}
	for _, a := range e.accounts {
		if seen[a.ID] {
			continue
		}
		seen[a.ID] = true
		perProvider[a.Provider]++
		l := []string{a.Provider, a.ID, a.Name}
		gauge(ch, descAccountInfo, 1, append(l, a.Platform, a.Status, strconv.FormatBool(a.Schedulable))...)

		g := e.groups[a.GroupKey]
		if g == nil {
			continue
		}
		until, blacklisted := e.blacklist[g.keyHash]
		blacklisted = blacklisted && now.Before(until)
		gauge(ch, descBlacklisted, boolF(blacklisted), l...)
		if blacklisted {
			gauge(ch, descBlacklistUntil, unixF(until), l...)
		}
		if !g.attempted {
			continue
		}
		gauge(ch, descUp, boolF(g.up), l...)
		gauge(ch, descAuthValid, boolF(!g.authFailed), l...)
		gauge(ch, descSupported, boolF(!g.unsupported), l...)
		gauge(ch, descLastAttempt, unixF(g.lastAttempt), l...)
		gauge(ch, descDuration, g.lastDuration.Seconds(), l...)
		gauge(ch, descHTTPStatus, float64(g.lastStatus), l...)
		if !g.lastSuccess.IsZero() {
			gauge(ch, descLastSuccess, unixF(g.lastSuccess), l...)
		}

		res := g.result
		if res == nil {
			continue
		}
		if max := e.cfg.Collect.MaxStaleness; max > 0 && now.Sub(g.lastSuccess) > max {
			continue
		}
		for _, t := range res.Tiers {
			gauge(ch, descUsedRatio, t.UsedPercent/100, append(l, t.Window)...)
			if t.ResetAt != "" {
				if ts, err := time.Parse(time.RFC3339, t.ResetAt); err == nil {
					gauge(ch, descReset, unixF(ts), append(l, t.Window)...)
				}
			}
			if t.Used != nil {
				gauge(ch, descWindowUsed, *t.Used, append(l, t.Window, t.Unit)...)
			}
			if t.Limit != nil {
				gauge(ch, descWindowLimit, *t.Limit, append(l, t.Window, t.Unit)...)
			}
		}
		for _, b := range res.Balances {
			gauge(ch, descBalance, b.Amount, append(l, b.Kind, b.Currency)...)
		}
		if res.BalanceAvailable != nil {
			gauge(ch, descBalanceAvailable, boolF(*res.BalanceAvailable), l...)
		}
		if res.Plan != "" {
			gauge(ch, descPlan, 1, append(l, res.Plan)...)
		}
	}
	for id, n := range perProvider {
		gauge(ch, descAccounts, n, id)
	}
}
