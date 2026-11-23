package usage

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/prometheus/common/expfmt"
	"github.com/stretchr/testify/require"

	"github.com/KexiChanProjectAI/llm-usage-exporter/internal/config"
)

// fakeSource keeps raw "log rows" (one Agg at an instant) and aggregates them
// per hour in the zone, as the SQL does.
type fakeSource struct {
	logs  []fakeLog
	since []time.Time
	fail  bool
	accs  map[int64]AccountMeta
}

type fakeLog struct {
	account int64
	at      time.Time
	agg     Agg
}

func (f *fakeSource) Hours(_ context.Context, since time.Time, tz string) ([]Row, error) {
	f.since = append(f.since, since)
	if f.fail {
		return nil, context.DeadlineExceeded
	}
	loc, _ := time.LoadLocation(tz)
	var out []Row
	for _, l := range f.logs {
		if l.at.Before(since) {
			continue
		}
		t := l.at.In(loc)
		out = append(out, Row{AccountID: l.account, Hour: time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, loc), Agg: l.agg})
	}
	return out, nil
}

func (f *fakeSource) Accounts(context.Context) (map[int64]AccountMeta, error) { return f.accs, nil }

func cfg(t *testing.T, yaml string) config.Usage {
	c, err := config.Parse([]byte("database: {dsn: postgres://x}\n" + yaml))
	require.NoError(t, err)
	return c.Usage
}

func newCol(t *testing.T, yaml string, src Source, now *time.Time) *Collector {
	c, err := New(cfg(t, yaml), src, func() map[string]string { return map[string]string{"1": "zhipu"} },
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	c.now = func() time.Time { return *now }
	return c
}

func tok(in, out float64, cost float64) Agg {
	return Agg{Requests: 1, Input: in, Output: out, TotalCost: cost, ActualCost: 2 * cost, AccountCost: cost}
}

func TestWindowsAndBuckets(t *testing.T) {
	now := time.Date(2026, 11, 23, 10, 30, 0, 0, time.UTC) // 18:30 in Shanghai
	src := &fakeSource{accs: map[int64]AccountMeta{1: {"glm-a", "zhipu"}, 2: {"claude-max", "anthropic"}}}
	src.logs = []fakeLog{
		{1, now.Add(-10 * time.Minute), tok(100, 10, 1)},   // this hour
		{1, now.Add(-20 * time.Hour), tok(200, 20, 2)},     // 22:30 on the 22nd in Shanghai
		{1, now.Add(-3 * 24 * time.Hour), tok(400, 40, 4)}, // 7d only
		{2, now.Add(-40 * 24 * time.Hour), tok(1, 1, 1)},   // outside every window
		{2, now.Add(-5 * time.Hour), tok(1000, 0, 10)},
	}
	c := newCol(t, "usage: {timezone: Asia/Shanghai, windows: [1, 7]}", src, &now)
	c.Refresh(context.Background())

	snap := c.snapshot()
	win := map[string]Agg{}
	for _, w := range snap.Windows {
		win[w.Provider+"/"+w.AccountName+"/"+w.Window] = w.Agg
	}
	require.Equal(t, 300.0, win["zhipu/glm-a/1d"].Input)
	require.Equal(t, 700.0, win["zhipu/glm-a/7d"].Input)
	require.Equal(t, 7.0, win["zhipu/glm-a/7d"].TotalCost)
	require.Equal(t, 14.0, win["zhipu/glm-a/7d"].ActualCost)
	require.Equal(t, 1000.0, win["anthropic/claude-max/1d"].Input, "untracked accounts use their platform")

	hours, days := map[string]float64{}, map[string]float64{}
	nh, nd := 0, 0
	for _, b := range snap.Buckets {
		if b.Granularity == "hour" {
			nh++
			hours[b.Provider+" "+b.Label] = b.Input
		} else {
			nd++
			days[b.Provider+" "+b.Label] = b.Input
		}
	}
	require.Equal(t, 2*24, nh, "24 zero-filled hours per provider")
	require.Equal(t, 2*7, nd)
	require.Equal(t, 100.0, hours["zhipu 2026-11-23 18:00"])
	require.Equal(t, 200.0, hours["zhipu 2026-11-22 22:00"])
	require.Equal(t, 0.0, hours["zhipu 2026-11-23 17:00"])
	require.Equal(t, 100.0, days["zhipu 2026-11-23"])
	require.Equal(t, 200.0, days["zhipu 2026-11-22"])
	require.Equal(t, 400.0, days["zhipu 2026-11-20"])
	require.Equal(t, 1000.0, days["anthropic 2026-11-23"])
}

func TestIncrementalReplacesRecentHours(t *testing.T) {
	now := time.Date(2026, 11, 23, 10, 30, 0, 0, time.UTC)
	src := &fakeSource{accs: map[int64]AccountMeta{1: {"glm-a", "zhipu"}}}
	src.logs = []fakeLog{
		{1, now.Add(-10 * time.Minute), tok(100, 0, 1)},
		{1, now.Add(-5 * time.Hour), tok(50, 0, 1)},
	}
	c := newCol(t, "usage: {windows: [1]}", src, &now)
	c.Refresh(context.Background())

	// New rows in this hour and the previous one; an old hour that changed is
	// not re-read until the resync.
	now = now.Add(5 * time.Minute)
	src.logs = append(src.logs,
		fakeLog{1, now.Add(-1 * time.Minute), tok(1, 0, 0)},
		fakeLog{1, now.Add(-50 * time.Minute), tok(10, 0, 0)},
		fakeLog{1, now.Add(-4 * time.Hour), tok(1000, 0, 0)})
	c.Refresh(context.Background())
	require.Equal(t, time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC), src.since[1])
	require.Equal(t, 161.0, c.snapshot().Windows[0].Input)

	now = now.Add(6 * time.Hour)
	c.Refresh(context.Background())
	require.Equal(t, 1161.0, c.snapshot().Windows[0].Input, "a resync reads the whole span")
}

func TestFailureKeepsValues(t *testing.T) {
	now := time.Date(2026, 11, 23, 10, 30, 0, 0, time.UTC)
	src := &fakeSource{accs: map[int64]AccountMeta{1: {"glm-a", "zhipu"}}, fail: true}
	c := newCol(t, "", src, &now)
	c.Refresh(context.Background())
	out, err := testutil.CollectAndFormat(c, expfmt.TypeTextPlain, "llm_usage_up", "llm_usage_tokens")
	require.NoError(t, err)
	require.Contains(t, string(out), "llm_usage_up 0")
	require.NotContains(t, string(out), "llm_usage_tokens", "no zeros before the first successful read")

	src.fail = false
	src.logs = []fakeLog{{1, now, tok(5, 0, 0)}}
	c.Refresh(context.Background())
	src.fail = true
	now = now.Add(5 * time.Minute)
	c.Refresh(context.Background())
	out, err = testutil.CollectAndFormat(c, expfmt.TypeTextPlain, "llm_usage_up", "llm_usage_tokens")
	require.NoError(t, err)
	require.Contains(t, string(out), "llm_usage_up 0")
	require.True(t, strings.Contains(string(out),
		`llm_usage_tokens{account_id="1",account_name="glm-a",provider="zhipu",type="input",window="7d"} 5`), string(out))
}

func TestDSTFallBackHasNoDuplicateBuckets(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC) // US DST ended at 06:00 UTC
	src := &fakeSource{accs: map[int64]AccountMeta{1: {"glm-a", "zhipu"}}, logs: []fakeLog{{1, now, tok(1, 0, 0)}}}
	c := newCol(t, "usage: {timezone: America/New_York}", src, &now)
	c.Refresh(context.Background())
	reg := prometheus.NewPedanticRegistry()
	require.NoError(t, reg.Register(c))
	_, err := reg.Gather() // fails on duplicate series
	require.NoError(t, err)
}
