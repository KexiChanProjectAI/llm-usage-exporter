package collector

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/KexiChanProjectAI/llm-usage-exporter/internal/provider"
	"github.com/KexiChanProjectAI/llm-usage-exporter/internal/source"
	"github.com/KexiChanProjectAI/llm-usage-exporter/internal/usage"
)

func rfc(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func TestSpanOf(t *testing.T) {
	at := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name         string
		tier         provider.Tier
		ok           bool
		start, until time.Time
	}{
		{"first use, 2h left", provider.Tier{Window: "5h", ResetAt: rfc(at.Add(2 * time.Hour))}, true,
			at.Add(-3 * time.Hour), at},
		{"weekly", provider.Tier{Window: "weekly", ResetAt: rfc(at.Add(24 * time.Hour))}, true,
			at.Add(-6 * 24 * time.Hour), at},
		{"idle window without reset", provider.Tier{Window: "5h", UsedPercent: 0}, false, time.Time{}, time.Time{}},
		{"ended before the fetch", provider.Tier{Window: "5h", ResetAt: rfc(at.Add(-time.Hour))}, true,
			at.Add(-6 * time.Hour), at.Add(-time.Hour)},
		{"reset beyond one length", provider.Tier{Window: "5h", ResetAt: rfc(at.Add(6 * time.Hour))}, false, time.Time{}, time.Time{}},
		{"monthly is not derived", provider.Tier{Window: "monthly", ResetAt: rfc(at.Add(time.Hour))}, false, time.Time{}, time.Time{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w, ok := spanOf(c.tier, at)
			require.Equal(t, c.ok, ok)
			if ok {
				require.True(t, c.start.Equal(w.Start), "start %s", w.Start)
				require.True(t, c.until.Equal(w.Until), "until %s", w.Until)
			}
		})
	}
}

func TestWindowConsumedIsReadAtFetchForTheWholeKey(t *testing.T) {
	h := newHarness(t, "accounts: [{name: cfgkimi, provider: kimi, api_key: k1}]\n")
	// Accounts 1 and 2 share a key with the config-file account; 3 has its own.
	h.accounts = append([]source.Account{kimi(1, "a", "k1"), kimi(2, "b", "k1"), kimi(3, "c", "k2")},
		source.StaticAccounts(h.e.cfg.Accounts)...)
	var fetchedAt time.Time
	h.e.fetch = func(_ context.Context, _ *http.Client, tg provider.Target) (*provider.Result, error) {
		fetchedAt = h.now
		return &provider.Result{Tiers: []provider.Tier{
			{Window: "5h", UsedPercent: 25, ResetAt: rfc(h.now.Add(4 * time.Hour))},
			{Window: "weekly", UsedPercent: 50},
			{Window: "monthly", UsedPercent: 10, ResetAt: rfc(h.now.Add(48 * time.Hour))},
		}}, nil
	}
	type call struct {
		ids   []int64
		spans []usage.Span
	}
	var calls []call
	h.e.SetWindowUsage(func(_ context.Context, ids []int64, spans []usage.Span) ([]usage.Consumed, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		calls = append(calls, call{ids, spans})
		if ids[0] == 3 {
			return nil, errors.New("db down")
		}
		return []usage.Consumed{{Requests: 10, Tokens: 1e6, Value: 2.5}}, nil
	})
	h.step(31*time.Second, true)

	require.Len(t, calls, 2)
	for _, c := range calls {
		require.Len(t, c.spans, 1, "only the 5h window has a derivable start")
		require.True(t, c.spans[0].Since.Equal(fetchedAt.Add(-time.Hour)))
		require.True(t, c.spans[0].Until.Equal(fetchedAt))
		if c.ids[0] != 3 {
			require.Equal(t, []int64{1, 2}, c.ids, "the config-file account has no usage_logs rows")
		}
	}

	out := h.metric("llm_quota_window_consumed")
	require.Contains(t, out, "account_id=1,account_name=a,provider=kimi,unit=usd,window=5h 2.5")
	require.Contains(t, out, "account_id=2,account_name=b,provider=kimi,unit=tokens,window=5h 1e+06")
	require.Contains(t, out, "account_id=cfg:cfgkimi,account_name=cfgkimi,provider=kimi,unit=requests,window=5h 10")
	require.NotContains(t, out, "account_id=3,", "no usage when the read failed")
	require.NotContains(t, out, "weekly")

	starts := h.metric("llm_quota_window_start_timestamp_seconds")
	require.Contains(t, starts, "account_id=3,account_name=c,provider=kimi,window=5h")
	require.NotContains(t, starts, "monthly")
}
