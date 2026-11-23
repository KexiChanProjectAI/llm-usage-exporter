package collector

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/KexiChanProjectAI/llm-usage-exporter/internal/config"
	"github.com/KexiChanProjectAI/llm-usage-exporter/internal/provider"
	"github.com/KexiChanProjectAI/llm-usage-exporter/internal/source"
)

type harness struct {
	t        *testing.T
	e        *Exporter
	now      time.Time
	accounts []source.Account
	mu       sync.Mutex
	calls    map[string]int // api key -> fetch count
	status   map[string]int // api key -> HTTP status to fail with
}

func newHarness(t *testing.T, yaml string) *harness {
	cfg, err := config.Parse([]byte("database: {dsn: postgres://x}\n" + yaml))
	require.NoError(t, err)
	h := &harness{t: t, now: time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC), calls: map[string]int{}, status: map[string]int{}}
	load := func(context.Context) ([]source.Account, error) { return h.accounts, nil }
	fetch := func(_ context.Context, _ *http.Client, tg provider.Target) (*provider.Result, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.calls[tg.APIKey]++
		if s := h.status[tg.APIKey]; s != 0 {
			if s == 404 {
				return nil, fmt.Errorf("%w: %w", provider.ErrUnsupported, &provider.HTTPError{Status: s, Body: "unsupported API path"})
			}
			return nil, &provider.HTTPError{Status: s, Body: "denied"}
		}
		return &provider.Result{
			Tiers:    []provider.Tier{{Window: "5h", UsedPercent: 40, ResetAt: "2026-11-23T05:00:00Z"}},
			Balances: []provider.Balance{{Kind: "total", Amount: 3, Currency: "USD"}},
			Plan:     "Pro",
		}, nil
	}
	h.e = New(cfg, load, fetch, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.e.now = func() time.Time { return h.now }
	return h
}

func kimi(id int64, name, key string) source.Account {
	return source.Account{ID: id, Name: name, Platform: "kimi", Type: "apikey", Status: "active", Schedulable: true,
		Credentials: map[string]any{"api_key": key, "account_mode": "coding"}}
}

// step reloads when asked (at the current time), then advances the clock,
// dispatches due fetches and waits for them.
func (h *harness) step(d time.Duration, reload bool) {
	if reload {
		h.e.reload(context.Background())
	}
	h.now = h.now.Add(d)
	h.e.dispatch(context.Background())
	h.e.wg.Wait()
}

func (h *harness) count(key string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls[key]
}

func (h *harness) metric(name string) string {
	h.t.Helper()
	reg := prometheus.NewRegistry()
	reg.MustRegister(h.e)
	mfs, err := reg.Gather()
	require.NoError(h.t, err)
	var b strings.Builder
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			var labels []string
			for _, l := range m.GetLabel() {
				labels = append(labels, l.GetName()+"="+l.GetValue())
			}
			v := m.GetGauge().GetValue()
			if m.Counter != nil {
				v = m.GetCounter().GetValue()
			}
			b.WriteString(strings.Join(labels, ",") + " " + strconv.FormatFloat(v, 'g', -1, 64) + "\n")
		}
	}
	return b.String()
}

func TestSharedKeyIsFetchedOnceAndExportedPerAccount(t *testing.T) {
	h := newHarness(t, "")
	h.accounts = []source.Account{kimi(1, "a", "k1"), kimi(2, "b", "k1"), kimi(3, "c", "k2")}
	h.step(31*time.Second, true)
	require.Equal(t, 1, h.count("k1"))
	require.Equal(t, 1, h.count("k2"))

	out := h.metric("llm_quota_used_ratio")
	require.Contains(t, out, "account_id=1,account_name=a,provider=kimi,window=5h 0.4")
	require.Contains(t, out, "account_id=2,account_name=b,provider=kimi,window=5h 0.4")
	require.Contains(t, out, "account_id=3,account_name=c,provider=kimi,window=5h 0.4")
	require.Contains(t, h.metric("llm_quota_plan_info"), "plan=Pro")
	require.Contains(t, h.metric("llm_exporter_accounts"), "provider=kimi 3")

	// Not due again before the interval (5m ± 10%).
	h.step(4*time.Minute, false)
	require.Equal(t, 1, h.count("k1"))
	h.step(2*time.Minute, false)
	require.Equal(t, 2, h.count("k1"))
}

func TestUnauthorizedKeyIsBlacklistedUntilItChanges(t *testing.T) {
	h := newHarness(t, "")
	h.accounts = []source.Account{kimi(1, "a", "bad")}
	h.status["bad"] = 401
	h.step(31*time.Second, true)
	require.Equal(t, 1, h.count("bad"))
	require.Contains(t, h.metric("llm_quota_key_blacklisted"), "account_id=1,account_name=a,provider=kimi 1")
	require.Contains(t, h.metric("llm_quota_auth_valid"), " 0")
	require.Empty(t, h.metric("llm_quota_used_ratio"))

	// Hours pass with reloads: the same key is never retried.
	for range 10 {
		h.step(30*time.Minute, true)
	}
	require.Equal(t, 1, h.count("bad"))

	// The key changes in sub2api: the new key is fetched right away.
	h.accounts = []source.Account{kimi(1, "a", "good")}
	h.step(31*time.Second, true)
	require.Equal(t, 1, h.count("good"))
	require.Contains(t, h.metric("llm_quota_key_blacklisted"), " 0")
	require.Contains(t, h.metric("llm_quota_used_ratio"), "window=5h 0.4")
}

func TestBlacklistExpiresAfterDuration(t *testing.T) {
	h := newHarness(t, "auth_blacklist: {duration: 6h}\n")
	h.accounts = []source.Account{kimi(1, "a", "bad")}
	h.status["bad"] = 401
	h.step(31*time.Second, true)
	h.step(5*time.Hour+58*time.Minute, false)
	require.Equal(t, 1, h.count("bad"))
	h.step(3*time.Minute, false)
	require.Equal(t, 2, h.count("bad"))
}

func TestOtherErrorsAreRetriedAndKeepLastValue(t *testing.T) {
	h := newHarness(t, "collect: {max_staleness: 20m}\n")
	h.accounts = []source.Account{kimi(1, "a", "k")}
	h.step(31*time.Second, true)
	h.status["k"] = 500
	h.step(6*time.Minute, false)
	require.Equal(t, 2, h.count("k"))
	require.Contains(t, h.metric("llm_quota_up"), " 0")
	require.Contains(t, h.metric("llm_quota_auth_valid"), " 1")
	require.Contains(t, h.metric("llm_quota_used_ratio"), "0.4", "last good value is kept")
	require.Contains(t, h.metric("llm_quota_key_blacklisted"), " 0")

	h.step(6*time.Minute, false)
	h.step(6*time.Minute, false)
	h.step(6*time.Minute, false)
	require.Empty(t, h.metric("llm_quota_used_ratio"), "dropped after max_staleness")
}

func TestRemovedAccountsDisappear(t *testing.T) {
	h := newHarness(t, "")
	h.accounts = []source.Account{kimi(1, "a", "k"), kimi(2, "b", "k2")}
	h.step(31*time.Second, true)
	h.accounts = h.accounts[:1]
	h.step(time.Second, true)
	require.NotContains(t, h.metric("llm_quota_account_info"), "account_id=2")
	require.Len(t, h.e.groups, 1)
}

func TestDisabledProviderIsIgnored(t *testing.T) {
	h := newHarness(t, "providers: {kimi: {enabled: false}}\n")
	h.accounts = []source.Account{kimi(1, "a", "k")}
	h.step(31*time.Second, true)
	require.Equal(t, 0, h.count("k"))
	require.NotContains(t, h.metric("llm_exporter_accounts"), "provider=kimi")
}

func TestProxyPrecedence(t *testing.T) {
	acc := &source.Account{ProxyURL: "socks5://acct:1080"}
	none := &source.Account{}
	cases := []struct {
		yaml string
		acc  *source.Account
		want string
	}{
		{"", none, ""},
		{"", acc, "socks5://acct:1080"},
		{"proxy: http://global:3128\n", none, "http://global:3128"},
		{"proxy: http://global:3128\n", acc, "socks5://acct:1080"},
		{"proxy: http://global:3128\nuse_account_proxy: false\n", acc, "http://global:3128"},
		{"proxy: http://global:3128\nproviders: {kimi: {proxy: socks5h://p:1}}\n", acc, "socks5h://p:1"},
		{"proxy: http://global:3128\nproviders: {kimi: {proxy: direct}}\n", acc, ""},
		{"proxy: direct\n", none, ""},
	}
	for _, tc := range cases {
		h := newHarness(t, tc.yaml)
		require.Equal(t, tc.want, h.e.resolveProxy("kimi", tc.acc), tc.yaml)
	}
}

func TestCollectorIsConsistent(t *testing.T) {
	h := newHarness(t, "")
	h.accounts = []source.Account{kimi(1, "a", "k"), kimi(2, "a", "k"), kimi(2, "a", "k")} // duplicate name and row
	h.step(31*time.Second, true)
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(h.e)
	_, err := reg.Gather()
	require.NoError(t, err)
	problems, err := testutil.CollectAndLint(h.e)
	require.NoError(t, err)
	require.Empty(t, problems)
}

func TestUnsupportedEndpointIsNotAFailureAndRetriedLate(t *testing.T) {
	h := newHarness(t, "")
	h.accounts = []source.Account{{ID: 9, Name: "relay", Platform: "deepseek", Type: "apikey", Status: "active",
		Credentials: map[string]any{"api_key": "r", "base_url": "https://relay.example.com/v1"}}}
	h.status["r"] = 404
	h.step(31*time.Second, true)
	require.Equal(t, 1, h.count("r"))
	require.Contains(t, h.metric("llm_quota_endpoint_supported"), "account_id=9,account_name=relay,provider=deepseek 0")
	require.Contains(t, h.metric("llm_quota_key_blacklisted"), " 0")
	require.Contains(t, h.metric("llm_quota_fetch_total"), "result=unsupported 1")

	h.step(5*time.Hour, false)
	require.Equal(t, 1, h.count("r"), "not retried every interval")
	h.step(time.Hour+time.Minute, false)
	require.Equal(t, 2, h.count("r"))
}

func TestStaticAccountLabel(t *testing.T) {
	h := newHarness(t, "")
	h.accounts = source.StaticAccounts([]config.StaticAccount{{Name: "DeepSeek Cyber", Provider: "deepseek", APIKey: "s"}})
	h.step(31*time.Second, true)
	require.Contains(t, h.metric("llm_quota_balance"), "account_id=cfg:DeepSeek Cyber,account_name=DeepSeek Cyber")
	require.Contains(t, h.metric("llm_quota_account_info"), "status=config")
}
