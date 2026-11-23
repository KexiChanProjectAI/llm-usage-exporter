package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// fakeUpstream answers requests by "host+path" without touching the network
// and records every request it saw.
type fakeUpstream struct {
	routes map[string]func(r *http.Request) (int, string)
	seen   []*http.Request
}

func (f *fakeUpstream) RoundTrip(r *http.Request) (*http.Response, error) {
	f.seen = append(f.seen, r)
	h, ok := f.routes[r.URL.Host+r.URL.Path]
	if !ok {
		return nil, errors.New("unexpected request to " + r.URL.String())
	}
	status, body := h(r)
	rec := httptest.NewRecorder()
	rec.WriteHeader(status)
	_, _ = rec.WriteString(body)
	resp := rec.Result()
	resp.Request = r
	return resp, nil
}

func newFake(routes map[string]func(r *http.Request) (int, string)) (*fakeUpstream, *http.Client) {
	f := &fakeUpstream{routes: routes}
	return f, &http.Client{Transport: f}
}

func fixed(status int, body string) func(*http.Request) (int, string) {
	return func(*http.Request) (int, string) { return status, body }
}

func TestFetchZhipuTeamPlan(t *testing.T) {
	f, client := newFake(map[string]func(*http.Request) (int, string){
		"api.z.ai/api/monitor/usage/quota/limit": fixed(200, `{"success":true,"data":{"level":"pro","limits":[
			{"type":"TOKENS_LIMIT","unit":3,"percentage":12,"nextResetTime":1700000000000},
			{"type":"TOKENS_LIMIT","unit":6,"percentage":34,"nextResetTime":1700600000000}]}}`),
	})
	res, err := Fetch(context.Background(), client, Target{
		Provider: "zhipu", APIKey: "k1", BaseURL: "https://api.z.ai/api/coding/paas/v4",
		ZhipuOrg: "org-1", ZhipuProject: "proj-1", HeaderOverrides: map[string]string{"x-extra": "v"},
	})
	require.NoError(t, err)
	require.Equal(t, "pro", res.Plan)
	require.Len(t, res.Tiers, 2)
	req := f.seen[0]
	require.Equal(t, "k1", req.Header.Get("Authorization"), "GLM takes the bare key")
	require.Equal(t, "type=2", req.URL.RawQuery)
	require.Equal(t, "org-1", req.Header.Get("bigmodel-organization"))
	require.Equal(t, "proj-1", req.Header.Get("bigmodel-project"))
	require.Equal(t, "v", req.Header.Get("X-Extra"))
}

func TestFetchZhipuBusinessError(t *testing.T) {
	_, client := newFake(map[string]func(*http.Request) (int, string){
		"open.bigmodel.cn/api/monitor/usage/quota/limit": fixed(200, `{"success":false,"msg":"no coding plan"}`),
	})
	_, err := Fetch(context.Background(), client, Target{Provider: "zhipu", APIKey: "k", BaseURL: "https://open.bigmodel.cn/api/coding/paas/v4"})
	require.ErrorContains(t, err, "no coding plan")
	require.Equal(t, 0, StatusOf(err), "a business error is not an HTTP status")
}

func TestFetchKimiAndUnauthorized(t *testing.T) {
	f, client := newFake(map[string]func(*http.Request) (int, string){
		"api.kimi.com/coding/v1/usages": func(r *http.Request) (int, string) {
			if r.Header.Get("Authorization") != "Bearer good" {
				return 401, `{"error":"invalid key"}`
			}
			return 200, `{"usages":{"limit_5h":{"used_ratio":0.25,"reset_time":"2026-11-23T10:00:00Z"}}}`
		},
	})
	res, err := Fetch(context.Background(), client, Target{Provider: "kimi", APIKey: "good", BaseURL: "https://api.kimi.com/coding"})
	require.NoError(t, err)
	require.Equal(t, []Tier{{Window: "5h", UsedPercent: 25, ResetAt: "2026-11-23T10:00:00Z"}}, res.Tiers)
	require.Equal(t, "Bearer good", f.seen[0].Header.Get("Authorization"))

	_, err = Fetch(context.Background(), client, Target{Provider: "kimi", APIKey: "bad", BaseURL: "https://api.kimi.com/coding"})
	require.Equal(t, 401, StatusOf(err))
}

func TestFetchNoWindowsIsFailure(t *testing.T) {
	_, client := newFake(map[string]func(*http.Request) (int, string){
		"api.kimi.com/coding/v1/usages": fixed(200, `{"limits_v2":[]}`),
		"opencode.ai/zen/go/v1/usage":   fixed(200, `{"ok":true}`),
	})
	_, err := Fetch(context.Background(), client, Target{Provider: "kimi", APIKey: "k", BaseURL: "https://api.kimi.com/coding/v1"})
	require.ErrorIs(t, err, ErrNoWindows)
	_, err = Fetch(context.Background(), client, Target{Provider: "opencode_go", APIKey: "k"})
	require.ErrorIs(t, err, ErrNoWindows)
}

func TestFetchMiniMax(t *testing.T) {
	_, client := newFake(map[string]func(*http.Request) (int, string){
		"api.minimax.io/v1/api/openplatform/coding_plan/remains": fixed(200, `{"base_resp":{"status_code":0},
			"current_subscribe_title":"Plus","model_remains":[{"model_name":"general","current_interval_remaining_percent":70,"end_time":1700000000000}]}`),
		"api.minimaxi.com/v1/api/openplatform/coding_plan/remains": fixed(200, `{"base_resp":{"status_code":1004,"status_msg":"bad key"}}`),
	})
	res, err := Fetch(context.Background(), client, Target{Provider: "minimax", APIKey: "k", BaseURL: "https://api.minimax.io/v1"})
	require.NoError(t, err)
	require.Equal(t, "Plus", res.Plan)
	require.InDelta(t, 30, res.Tiers[0].UsedPercent, 1e-9)

	_, err = Fetch(context.Background(), client, Target{Provider: "minimax", APIKey: "k", BaseURL: "https://api.minimaxi.com/v1"})
	require.ErrorContains(t, err, "bad key")
}

func TestFetchOpenCodeGoUsesOfficialURL(t *testing.T) {
	f, client := newFake(map[string]func(*http.Request) (int, string){
		"opencode.ai/zen/go/v1/usage": fixed(200, `{"usage":{"rolling":{"percent":5,"resetsAt":"2026-11-23T10:00:00Z"},"monthly":{"percent":50}}}`),
	})
	res, err := Fetch(context.Background(), client, Target{Provider: "opencode_go", APIKey: "k", BaseURL: "https://relay.example.com/v1"})
	require.NoError(t, err)
	require.Len(t, res.Tiers, 2)
	require.Equal(t, "opencode.ai", f.seen[0].URL.Host)
}

func ccRoutes(org string, subStatus int) map[string]func(*http.Request) (int, string) {
	whoami := `{"user":{"id":"u"},"org":null}`
	if org != "" {
		whoami = `{"user":{"id":"u"},"org":{"id":"` + org + `"}}`
	}
	return map[string]func(*http.Request) (int, string){
		"api.commandcode.ai/alpha/whoami": fixed(200, whoami),
		"api.commandcode.ai/alpha/billing/credits": fixed(200, `{"credits":{"monthlyCredits":"30","purchasedCredits":5,"freeCredits":null},
			"windowLimits":{"limited":true,"fiveHour":{"used":2,"cap":8,"resetAt":1700000000000},"weekly":{"used":10,"cap":40,"resetAt":1700600000000}}}`),
		"api.commandcode.ai/alpha/billing/subscriptions": fixed(subStatus, `{"data":{"planId":"individual-pro","currentPeriodStart":"2026-11-01T00:00:00Z","currentPeriodEnd":"2026-12-01T00:00:00Z"}}`),
		"api.commandcode.ai/alpha/usage/summary":         fixed(200, `{"totalCost":99,"totalMonthlyCredits":10}`),
	}
}

func TestFetchCommandCode(t *testing.T) {
	f, client := newFake(ccRoutes("org-9", 200))
	res, err := Fetch(context.Background(), client, Target{Provider: "command_code", APIKey: "k"})
	require.NoError(t, err)
	require.Equal(t, "Pro", res.Plan)
	require.Len(t, res.Tiers, 3)
	require.Equal(t, "5h", res.Tiers[0].Window)
	require.InDelta(t, 25, res.Tiers[0].UsedPercent, 1e-9)
	require.Equal(t, "usd", res.Tiers[0].Unit)
	monthly := res.Tiers[2]
	require.Equal(t, "monthly", monthly.Window)
	require.InDelta(t, 25, monthly.UsedPercent, 1e-9) // 10 / (10 + 30)
	require.InDelta(t, 40, *monthly.Limit, 1e-9)
	require.Equal(t, "2026-12-01T00:00:00Z", monthly.ResetAt)
	require.Equal(t, Balance{Kind: "total", Amount: 35, Currency: "USD"}, res.Balances[0])

	for _, r := range f.seen[1:] {
		require.Equal(t, "org-9", r.URL.Query().Get("orgId"), r.URL.Path)
	}
	require.Equal(t, "2026-11-01T00:00:00Z", f.seen[3].URL.Query().Get("since"))
}

func TestFetchCommandCodePersonalAndPeriodFailure(t *testing.T) {
	f, client := newFake(ccRoutes("", 500))
	res, err := Fetch(context.Background(), client, Target{Provider: "command_code", APIKey: "k"})
	require.NoError(t, err, "a failing subscription call only drops the monthly window")
	require.Len(t, res.Tiers, 2)
	for _, r := range f.seen {
		require.False(t, r.URL.Query().Has("orgId"), "personal accounts must not send orgId")
	}

	_, client = newFake(ccRoutes("", 403))
	_, err = Fetch(context.Background(), client, Target{Provider: "command_code", APIKey: "k"})
	require.Equal(t, 403, StatusOf(err))
}

func TestFetchCline(t *testing.T) {
	_, client := newFake(map[string]func(*http.Request) (int, string){
		"api.cline.bot/api/v1/users/me/plan/usage-limits": fixed(200, `{"success":true,"data":{"limits":[
			{"type":"five_hour","percentUsed":12.5,"resetsAt":"2026-11-23T10:00:00.123Z"},{"type":"monthly","percentUsed":3}]}}`),
		"api.cline.bot/api/v1/users/me":             fixed(200, `{"success":true,"data":{"id":"user/1"}}`),
		"api.cline.bot/api/v1/users/user/1/balance": fixed(200, `{"success":true,"data":{"balance":12345678}}`),
	})
	res, err := Fetch(context.Background(), client, Target{Provider: "cline", APIKey: "k"})
	require.NoError(t, err)
	require.Equal(t, "ClinePass", res.Plan)
	require.Len(t, res.Tiers, 2)
	require.Equal(t, "2026-11-23T10:00:00Z", res.Tiers[0].ResetAt)
	require.InDelta(t, 12.3457, res.Balances[0].Amount, 1e-9)
}

func TestFetchClineNotSubscribed(t *testing.T) {
	_, client := newFake(map[string]func(*http.Request) (int, string){
		"api.cline.bot/api/v1/users/me/plan/usage-limits": fixed(404, `{"success":false,"error":"no plan history found for user"}`),
		"api.cline.bot/api/v1/users/me":                   fixed(200, `{"success":true,"data":{"id":"u1"}}`),
		"api.cline.bot/api/v1/users/u1/balance":           fixed(200, `{"success":true,"data":{"balance":"2000000"}}`),
	})
	res, err := Fetch(context.Background(), client, Target{Provider: "cline", APIKey: "k"})
	require.NoError(t, err)
	require.Empty(t, res.Tiers)
	require.Empty(t, res.Plan)
	require.InDelta(t, 2, res.Balances[0].Amount, 1e-9)
}

func TestRedirectIsNotFollowedAndBodyIsBounded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/big" {
			_, _ = io.WriteString(w, strings.Repeat("x", maxBodyBytes+10))
			return
		}
		http.Redirect(w, r, "https://evil.example.com/", http.StatusFound)
	}))
	defer srv.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	_, err := get(context.Background(), client, Target{APIKey: "k"}, request{url: srv.URL + "/r", auth: "Bearer k"})
	require.Equal(t, http.StatusFound, StatusOf(err))
	_, err = get(context.Background(), client, Target{APIKey: "k"}, request{url: srv.URL + "/big", auth: "Bearer k"})
	require.ErrorIs(t, err, ErrInvalidResponse)
}

func TestFetchDeepSeekBalance(t *testing.T) {
	f, client := newFake(map[string]func(*http.Request) (int, string){
		"api.deepseek.com/user/balance": fixed(200, `{"is_available":false,"balance_infos":[
			{"currency":"CNY","total_balance":"110.00","granted_balance":"10.00","topped_up_balance":"100.00"},
			{"currency":"usd","total_balance":"2.5"},
			{"currency":"EUR","total_balance":"oops"}]}`),
	})
	res, err := Fetch(context.Background(), client, Target{Provider: "deepseek", APIKey: "k", BaseURL: "https://api.deepseek.com"})
	require.NoError(t, err)
	require.Equal(t, "Bearer k", f.seen[0].Header.Get("Authorization"))
	require.Equal(t, []Balance{
		{Kind: "total", Amount: 110, Currency: "CNY"},
		{Kind: "granted", Amount: 10, Currency: "CNY"},
		{Kind: "topped_up", Amount: 100, Currency: "CNY"},
		{Kind: "total", Amount: 2.5, Currency: "USD"},
	}, res.Balances)
	require.NotNil(t, res.BalanceAvailable)
	require.False(t, *res.BalanceAvailable)
	require.Empty(t, res.Tiers)
}

func TestFetchDeepSeekInvalid(t *testing.T) {
	_, client := newFake(map[string]func(*http.Request) (int, string){
		"api.deepseek.com/user/balance": fixed(200, `{"balance_infos":[]}`),
	})
	_, err := Fetch(context.Background(), client, Target{Provider: "deepseek", APIKey: "k", BaseURL: "https://api.deepseek.com/"})
	require.ErrorIs(t, err, ErrInvalidResponse)
}

func TestFetchDeepSeekRelayWithoutBalanceAPI(t *testing.T) {
	cases := map[string]struct {
		status      int
		body        string
		unsupported bool
	}{
		"chat gateway 400":     {400, `{"error":{"message":"request must contain model"}}`, true},
		"404":                  {404, `{"error":{"message":"unsupported API path"}}`, true},
		"503 wrapping 404":     {503, `{"error":{"message":"Not Found (session x)"}}`, true},
		"200 something else":   {200, `{"object":"chat.completion"}`, true},
		"401 is a key problem": {401, `{"error":"invalid key"}`, false},
		"429 is a limit":       {429, `{"error":"not found"}`, false},
		"plain 500":            {500, `{"error":"boom"}`, false},
	}
	for name, tc := range cases {
		_, client := newFake(map[string]func(*http.Request) (int, string){
			"relay.example.com/v1/user/balance": fixed(tc.status, tc.body),
		})
		_, err := Fetch(context.Background(), client, Target{Provider: "deepseek", APIKey: "k", BaseURL: "https://relay.example.com/v1"})
		require.Error(t, err, name)
		require.Equal(t, tc.unsupported, errors.Is(err, ErrUnsupported), name)
	}
}
