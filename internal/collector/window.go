package collector

import (
	"context"
	"sort"
	"time"

	"github.com/KexiChanProjectAI/llm-usage-exporter/internal/provider"
	"github.com/KexiChanProjectAI/llm-usage-exporter/internal/usage"
)

// windowLength is the length of the quota windows whose start can be derived
// from their reset time. Monthly windows are left out: whether they are a
// calendar month, 30 days or a billing period differs by provider.
var windowLength = map[string]time.Duration{
	"5h":     5 * time.Hour,
	"weekly": 7 * 24 * time.Hour,
}

// quotaWindow is the current instance of one quota window.
type quotaWindow struct {
	// Start is the reset time minus the window length. It holds whether the
	// provider starts windows on a fixed schedule (MiniMax) or at the first
	// request after the previous one ended (GLM): in both, the window that
	// resets at R began at R minus its length.
	Start time.Time
	// Until is the end of the span Consumed covers: the fetch time, or the
	// reset time when the window had already ended by then.
	Until    time.Time
	Consumed *usage.Consumed // nil when the usage could not be read
}

// spanOf returns the span of a tier, or false when it has none:
//   - no reset time: the window has not started (GLM reports an idle 5h
//     window as 0% with no reset) or the provider does not say;
//   - the reset is more than one window length after the fetch: the
//     assumed length does not fit this provider.
func spanOf(t provider.Tier, at time.Time) (quotaWindow, bool) {
	length, ok := windowLength[t.Window]
	if !ok || t.ResetAt == "" {
		return quotaWindow{}, false
	}
	reset, err := time.Parse(time.RFC3339, t.ResetAt)
	if err != nil {
		return quotaWindow{}, false
	}
	start := reset.Add(-length)
	if start.After(at) {
		return quotaWindow{}, false
	}
	until := at
	if reset.Before(until) {
		until = reset
	}
	return quotaWindow{Start: start, Until: until}, true
}

// quotaWindows derives the window spans of a fresh result and, when window
// usage is enabled, reads what the group's sub2api accounts logged in each.
// The usage is read right after the fetch so that it pairs with the used
// ratio the provider just reported.
func (e *Exporter) quotaWindows(ctx context.Context, g *group, res *provider.Result, at time.Time) map[string]quotaWindow {
	out := map[string]quotaWindow{}
	var (
		names []string
		spans []usage.Span
	)
	for _, t := range res.Tiers {
		w, ok := spanOf(t, at)
		if !ok {
			continue
		}
		if _, dup := out[t.Window]; dup {
			continue
		}
		out[t.Window] = w
		names = append(names, t.Window)
		spans = append(spans, usage.Span{Since: w.Start, Until: w.Until})
	}
	if len(out) == 0 {
		return nil
	}
	if e.usage == nil {
		return out
	}
	ids := e.usageIDs(g.key)
	if len(ids) == 0 {
		return out // config-file accounts only: nothing in usage_logs
	}
	qctx, cancel := context.WithTimeout(ctx, e.cfg.Database.QueryTimeout)
	defer cancel()
	consumed, err := e.usage(qctx, ids, spans)
	if err != nil || len(consumed) != len(spans) {
		if ctx.Err() == nil {
			e.log.Warn("read usage since the quota windows started failed; window usage is not exported until the next fetch",
				"provider", g.target.Provider, "accounts", ids, "error", err)
		}
		return out
	}
	for i, name := range names {
		w := out[name]
		c := consumed[i]
		w.Consumed = &c
		out[name] = w
	}
	return out
}

// usageIDs returns the sub2api account IDs that share a fetch group. They use
// the same API key, so they draw from the same quota.
func (e *Exporter) usageIDs(key string) []int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	seen := map[int64]bool{}
	var ids []int64
	for _, a := range e.accounts {
		if a.GroupKey == key && a.UsageID != 0 && !seen[a.UsageID] {
			seen[a.UsageID] = true
			ids = append(ids, a.UsageID)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}
