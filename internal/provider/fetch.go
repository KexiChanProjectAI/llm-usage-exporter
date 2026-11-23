// Fetchers in this file port the quota probes of sub2api
// (github.com/Wei-Shaw/sub2api, backend/internal/service/
// cn_provider_quota_service.go, command_code_usage.go, cline_usage.go and
// opencode_go_usage.go, commit 328db26), licensed under the GNU LGPL v3.0.

package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	"github.com/KexiChanProjectAI/llm-usage-exporter/internal/config"
)

const maxBodyBytes = 512 * 1024

// UserAgent is sent on every upstream request.
var UserAgent = "llm-usage-exporter"

// Balance is a credit balance reported next to the windows.
type Balance struct {
	Kind     string // "total", "monthly", "purchased", "free"
	Amount   float64
	Currency string
}

// Result is one successful quota fetch.
type Result struct {
	Tiers    []Tier
	Balances []Balance
	Plan     string
	// BalanceAvailable is the provider's own "balance is usable" flag
	// (DeepSeek is_available); nil when the provider has none.
	BalanceAvailable *bool
}

// HTTPError is a non-2xx upstream response, or a 2xx response that carries a
// business-level error (Status is then the HTTP status, Business is true).
type HTTPError struct {
	Status   int
	Body     string
	Business bool
}

func (e *HTTPError) Error() string {
	if e.Business {
		return fmt.Sprintf("API error: %s", e.Body)
	}
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Body)
}

// ErrNoWindows means a 2xx response held no recognizable usage window, which
// usually means the upstream schema changed. It is a failure, not "0% used".
var ErrNoWindows = errors.New("response contains no recognized usage window (upstream schema may have changed)")

// ErrUnsupported means the account's host does not offer this endpoint (a
// relay without the vendor's quota or balance API). It is not a failure of
// the account and is retried rarely.
var ErrUnsupported = errors.New("endpoint not offered by this host")

// ErrInvalidResponse means the response body could not be decoded.
var ErrInvalidResponse = errors.New("invalid upstream response")

// StatusOf returns the HTTP status carried by err, or 0.
func StatusOf(err error) int {
	var httpErr *HTTPError
	if errors.As(err, &httpErr) && !httpErr.Business {
		return httpErr.Status
	}
	return 0
}

// Fetch queries the quota for t using client.
func Fetch(ctx context.Context, client *http.Client, t Target) (*Result, error) {
	switch t.Provider {
	case config.ProviderZhipu:
		return fetchZhipu(ctx, client, t)
	case config.ProviderKimi:
		return fetchKimi(ctx, client, t)
	case config.ProviderMiniMax:
		return fetchMiniMax(ctx, client, t)
	case config.ProviderOpenCodeGo:
		return fetchOpenCodeGo(ctx, client, t)
	case config.ProviderCommandCode:
		return fetchCommandCode(ctx, client, t)
	case config.ProviderCline:
		return fetchCline(ctx, client, t)
	case config.ProviderDeepSeek:
		return fetchDeepSeek(ctx, client, t)
	}
	return nil, fmt.Errorf("unsupported provider %q", t.Provider)
}

type request struct {
	url     string
	auth    string
	headers map[string]string
}

// get performs one GET and returns the body of a 2xx response.
func get(ctx context.Context, client *http.Client, t Target, r request) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", r.auth)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", UserAgent)
	for k, v := range r.headers {
		req.Header.Set(k, v)
	}
	// Same account-level overrides as sub2api's forwarding and probes.
	for k, v := range t.HeaderOverrides {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", scrubURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if len(body) > maxBodyBytes {
		return nil, fmt.Errorf("%w: response too large", ErrInvalidResponse)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &HTTPError{Status: resp.StatusCode, Body: truncate(strings.TrimSpace(string(body)), 240)}
	}
	return body, nil
}

// scrubURLError drops the URL (which may carry a proxy password) from
// *url.Error; the underlying cause is kept.
func scrubURLError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err
	}
	return err
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func fetchZhipu(ctx context.Context, client *http.Client, t Target) (*Result, error) {
	target := zhipuQuotaURL(t.BaseURL)
	headers := map[string]string{"Content-Type": "application/json", "Accept-Language": "en-US,en"}
	// Team GLM Coding Plan needs ?type=2 plus organization / project headers,
	// otherwise the API answers "user has no coding plan".
	if t.ZhipuOrg != "" {
		target += "?type=2"
		headers["bigmodel-organization"] = t.ZhipuOrg
		if t.ZhipuProject != "" {
			headers["bigmodel-project"] = t.ZhipuProject
		}
	}
	// The GLM quota endpoint takes the bare key, without "Bearer ".
	body, err := get(ctx, client, t, request{url: target, auth: t.APIKey, headers: headers})
	if err != nil {
		return nil, err
	}
	if success := gjson.GetBytes(body, "success"); success.Exists() && !success.Bool() {
		msg := strings.TrimSpace(gjson.GetBytes(body, "msg").String())
		if msg == "" {
			msg = "unknown zhipu quota error"
		}
		return nil, &HTTPError{Status: http.StatusOK, Body: truncate(msg, 240), Business: true}
	}
	tiers := parseZhipuTokenTiers(gjson.GetBytes(body, "data"))
	if len(tiers) == 0 {
		return nil, ErrNoWindows
	}
	return &Result{Tiers: tiers, Plan: strings.TrimSpace(gjson.GetBytes(body, "data.level").String())}, nil
}

func fetchKimi(ctx context.Context, client *http.Client, t Target) (*Result, error) {
	body, err := get(ctx, client, t, request{url: kimiQuotaURL(t.BaseURL), auth: "Bearer " + t.APIKey})
	if err != nil {
		return nil, err
	}
	tiers := parseKimiUsageTiers(body)
	if len(tiers) == 0 {
		return nil, ErrNoWindows
	}
	return &Result{Tiers: tiers}, nil
}

func fetchMiniMax(ctx context.Context, client *http.Client, t Target) (*Result, error) {
	body, err := get(ctx, client, t, request{
		url:     minimaxQuotaURL(t.BaseURL),
		auth:    "Bearer " + t.APIKey,
		headers: map[string]string{"Content-Type": "application/json", "Accept-Language": "en-US,en"},
	})
	if err != nil {
		return nil, err
	}
	if status := gjson.GetBytes(body, "base_resp.status_code"); status.Exists() && status.Int() != 0 {
		msg := strings.TrimSpace(gjson.GetBytes(body, "base_resp.status_msg").String())
		if msg == "" {
			msg = "unknown minimax quota error"
		}
		return nil, &HTTPError{Status: http.StatusOK, Body: truncate(fmt.Sprintf("(%d) %s", status.Int(), msg), 240), Business: true}
	}
	tiers := parseMiniMaxUsageTiers(body)
	if len(tiers) == 0 {
		return nil, ErrNoWindows
	}
	return &Result{Tiers: tiers, Plan: strings.TrimSpace(gjson.GetBytes(body, "current_subscribe_title").String())}, nil
}

// openCodeGoUsageURL is fixed: the key of an opencode_go account is an
// official OpenCode Go key even when its base_url points at a relay.
const openCodeGoUsageURL = "https://opencode.ai/zen/go/v1/usage"

func fetchOpenCodeGo(ctx context.Context, client *http.Client, t Target) (*Result, error) {
	body, err := get(ctx, client, t, request{url: openCodeGoUsageURL, auth: "Bearer " + t.APIKey})
	if err != nil {
		return nil, err
	}
	if !gjson.ValidBytes(body) {
		return nil, ErrInvalidResponse
	}
	tiers := parseOpenCodeGoUsageTiers(body)
	if len(tiers) == 0 {
		return nil, ErrNoWindows
	}
	return &Result{Tiers: tiers, Plan: "OpenCode Go"}, nil
}

// ---- Command Code ----
//
//	GET /alpha/whoami                          -> {user, org}; org is null for personal accounts
//	GET /alpha/billing/credits[?orgId=]        -> {credits:{monthlyCredits, purchasedCredits, freeCredits},
//	                                               windowLimits:{limited, fiveHour, weekly}}
//	GET /alpha/billing/subscriptions[?orgId=]  -> {data:{planId, currentPeriodStart, currentPeriodEnd}}
//	GET /alpha/usage/summary[?orgId=][&since=] -> {totalCost, totalMonthlyCredits}
//
// Credits are USD. fiveHour / weekly carry used and cap in credits and resetAt
// in epoch ms; limited=false (pay as you go) has no rolling window. The
// monthly cap is rebuilt as "used this period + remaining". Personal accounts
// must not send orgId (an empty one returns 400).

const commandCodeAPIBase = "https://api.commandcode.ai"

var commandCodePlanNames = map[string]string{
	"individual-go":       "Go",
	"individual-goat":     "GOAT",
	"individual-pro":      "Pro",
	"individual-pro-v1":   "Pro",
	"individual-provider": "Provider",
	"individual-max":      "Max",
	"individual-ultra":    "Ultra",
	"teams-pro":           "Teams Pro",
}

// ccNumber accepts numbers, numeric strings and null.
type ccNumber float64

func (n *ccNumber) UnmarshalJSON(data []byte) error {
	s := strings.TrimSpace(string(data))
	if s == "" || s == "null" {
		*n = 0
		return nil
	}
	if s[0] == '"' {
		var str string
		if err := json.Unmarshal(data, &str); err != nil {
			return err
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(str), 64)
		if err != nil {
			return err
		}
		*n = ccNumber(v)
		return nil
	}
	var v float64
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*n = ccNumber(v)
	return nil
}

type ccWindow struct {
	Used    ccNumber `json:"used"`
	Cap     ccNumber `json:"cap"`
	ResetAt ccNumber `json:"resetAt"`
}

type ccCreditsPayload struct {
	Credits *struct {
		MonthlyCredits   ccNumber `json:"monthlyCredits"`
		PurchasedCredits ccNumber `json:"purchasedCredits"`
		FreeCredits      ccNumber `json:"freeCredits"`
	} `json:"credits"`
	WindowLimits struct {
		Limited  bool      `json:"limited"`
		FiveHour *ccWindow `json:"fiveHour"`
		Weekly   *ccWindow `json:"weekly"`
	} `json:"windowLimits"`
}

type ccSubscriptionPayload struct {
	Data struct {
		PlanID             string `json:"planId"`
		CurrentPeriodStart string `json:"currentPeriodStart"`
		CurrentPeriodEnd   string `json:"currentPeriodEnd"`
	} `json:"data"`
}

type ccSummaryPayload struct {
	TotalCost           *ccNumber `json:"totalCost"`
	TotalMonthlyCredits *ccNumber `json:"totalMonthlyCredits"`
}

func ccGet(ctx context.Context, client *http.Client, t Target, path string, query url.Values, out any) error {
	target := commandCodeAPIBase + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	body, err := get(ctx, client, t, request{url: target, auth: "Bearer " + t.APIKey})
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%w: %s: %v", ErrInvalidResponse, path, err)
	}
	return nil
}

func ccPercent(used, limit float64) float64 {
	return math.Max(0, math.Round(used/limit*100*100)/100)
}

func fetchCommandCode(ctx context.Context, client *http.Client, t Target) (*Result, error) {
	// whoami decides the orgId; any failure aborts, since querying an org
	// account without orgId returns the member's personal wallet instead.
	var whoami struct {
		Org *struct {
			ID string `json:"id"`
		} `json:"org"`
	}
	if err := ccGet(ctx, client, t, "/alpha/whoami", nil, &whoami); err != nil {
		return nil, err
	}
	query := url.Values{}
	if whoami.Org != nil && strings.TrimSpace(whoami.Org.ID) != "" {
		query.Set("orgId", strings.TrimSpace(whoami.Org.ID))
	}
	var credits ccCreditsPayload
	if err := ccGet(ctx, client, t, "/alpha/billing/credits", query, &credits); err != nil {
		return nil, err
	}
	if credits.Credits == nil {
		return nil, fmt.Errorf("%w: credits missing", ErrInvalidResponse)
	}

	res := &Result{}
	if credits.WindowLimits.Limited {
		for _, item := range []struct {
			window string
			w      *ccWindow
		}{{"5h", credits.WindowLimits.FiveHour}, {"weekly", credits.WindowLimits.Weekly}} {
			if item.w == nil || item.w.Cap <= 0 {
				continue
			}
			res.Tiers = append(res.Tiers, Tier{
				Window:      item.window,
				UsedPercent: ccPercent(float64(item.w.Used), float64(item.w.Cap)),
				ResetAt:     millisToRFC3339(int64(item.w.ResetAt)),
				Used:        f64(float64(item.w.Used)),
				Limit:       f64(float64(item.w.Cap)),
				Unit:        "usd",
			})
		}
	}
	c := credits.Credits
	total := math.Round(float64(c.MonthlyCredits+c.PurchasedCredits+c.FreeCredits)*10000) / 10000
	res.Balances = []Balance{
		{Kind: "total", Amount: total, Currency: "USD"},
		{Kind: "monthly", Amount: float64(c.MonthlyCredits), Currency: "USD"},
		{Kind: "purchased", Amount: float64(c.PurchasedCredits), Currency: "USD"},
		{Kind: "free", Amount: float64(c.FreeCredits), Currency: "USD"},
	}

	// The monthly window is best effort: auth errors fail the fetch, anything
	// else only drops the monthly window.
	var sub ccSubscriptionPayload
	if err := ccGet(ctx, client, t, "/alpha/billing/subscriptions", query, &sub); err != nil {
		if isAuthStatus(StatusOf(err)) {
			return nil, err
		}
		return res, nil
	}
	planID := strings.TrimSpace(sub.Data.PlanID)
	res.Plan = planID
	if name, ok := commandCodePlanNames[planID]; ok {
		res.Plan = name
	}
	periodStart := strings.TrimSpace(sub.Data.CurrentPeriodStart)
	if periodStart == "" {
		return res, nil
	}
	summaryQuery := url.Values{}
	for k, v := range query {
		summaryQuery[k] = v
	}
	summaryQuery.Set("since", periodStart)
	var summary ccSummaryPayload
	if err := ccGet(ctx, client, t, "/alpha/usage/summary", summaryQuery, &summary); err != nil {
		if isAuthStatus(StatusOf(err)) {
			return nil, err
		}
		return res, nil
	}
	used := 0.0
	switch {
	case summary.TotalMonthlyCredits != nil:
		used = float64(*summary.TotalMonthlyCredits)
	case summary.TotalCost != nil:
		used = float64(*summary.TotalCost)
	}
	if limit := used + float64(c.MonthlyCredits); limit > 0 {
		res.Tiers = append(res.Tiers, Tier{
			Window:      "monthly",
			UsedPercent: ccPercent(used, limit),
			ResetAt:     normalizeResetTime(sub.Data.CurrentPeriodEnd),
			Used:        f64(used),
			Limit:       f64(limit),
			Unit:        "usd",
		})
	}
	return res, nil
}

func isAuthStatus(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden
}

// ---- Cline ----
//
//	GET /api/v1/users/me                     -> {id, email, ...}
//	GET /api/v1/users/{id}/balance           -> {balance, userId}
//	GET /api/v1/users/me/plan/usage-limits   -> {limits:[{type, percentUsed, resetsAt?}]}
//
// Responses are wrapped in {success, error, data}. balance is in millionths of
// a credit (1 credit = 1 USD). usage-limits types are five_hour / weekly /
// monthly; no subscription answers 404 or an empty list.

const clineAPIBase = "https://api.cline.bot"

func clineGet(ctx context.Context, client *http.Client, t Target, path string) (gjson.Result, error) {
	body, err := get(ctx, client, t, request{url: clineAPIBase + path, auth: "Bearer " + t.APIKey})
	if err != nil {
		return gjson.Result{}, err
	}
	if !gjson.ValidBytes(body) {
		return gjson.Result{}, fmt.Errorf("%w: %s", ErrInvalidResponse, path)
	}
	parsed := gjson.ParseBytes(body)
	if success := parsed.Get("success"); success.Exists() {
		if !success.Bool() {
			return gjson.Result{}, &HTTPError{Status: http.StatusOK, Body: truncate(parsed.Get("error").String(), 240), Business: true}
		}
		return parsed.Get("data"), nil
	}
	return parsed, nil
}

func fetchCline(ctx context.Context, client *http.Client, t Target) (*Result, error) {
	res := &Result{}
	data, err := clineGet(ctx, client, t, "/api/v1/users/me/plan/usage-limits")
	switch {
	case StatusOf(err) == http.StatusNotFound:
		// no ClinePass subscription: balance only
	case err != nil:
		return nil, err
	default:
		limits := data.Get("limits")
		if !limits.IsArray() {
			return nil, fmt.Errorf("%w: missing limits", ErrInvalidResponse)
		}
		windows := map[string]string{"five_hour": "5h", "weekly": "weekly", "monthly": "monthly"}
		for _, item := range limits.Array() {
			window, ok := windows[strings.ToLower(strings.TrimSpace(item.Get("type").String()))]
			used := item.Get("percentUsed")
			if !ok || used.Type != gjson.Number {
				continue
			}
			tier := Tier{Window: window, UsedPercent: used.Float()}
			if resetAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(item.Get("resetsAt").String())); err == nil {
				tier.ResetAt = resetAt.UTC().Format(time.RFC3339)
			}
			res.Tiers = append(res.Tiers, tier)
		}
		if len(res.Tiers) > 0 {
			res.Plan = "ClinePass"
		}
	}

	me, err := clineGet(ctx, client, t, "/api/v1/users/me")
	if err != nil {
		if isAuthStatus(StatusOf(err)) || len(res.Tiers) == 0 {
			return nil, err
		}
		return res, nil // windows are still valid without the balance
	}
	userID := strings.TrimSpace(me.Get("id").String())
	if userID == "" {
		if len(res.Tiers) == 0 {
			return nil, fmt.Errorf("%w: missing user id", ErrInvalidResponse)
		}
		return res, nil
	}
	bal, err := clineGet(ctx, client, t, "/api/v1/users/"+url.PathEscape(userID)+"/balance")
	if err != nil {
		if isAuthStatus(StatusOf(err)) || len(res.Tiers) == 0 {
			return nil, err
		}
		return res, nil
	}
	micro := bal.Get("balance")
	if !micro.Exists() || (micro.Type != gjson.Number && micro.Type != gjson.String) {
		if len(res.Tiers) == 0 {
			return nil, fmt.Errorf("%w: missing balance", ErrInvalidResponse)
		}
		return res, nil
	}
	res.Balances = []Balance{{Kind: "total", Amount: math.Round(micro.Float()/100) / 10000, Currency: "USD"}}
	return res, nil
}

// ---- DeepSeek ----
//
//	GET /user/balance -> {is_available, balance_infos:[{currency, total_balance,
//	                       granted_balance, topped_up_balance}]}
//
// Dual-currency accounts return one entry per currency (CNY and USD).
// Ported from sub2api cn_provider_balance_service.go.

// deepSeekBalanceURL appends /user/balance to the OpenAI-format base URL.
func deepSeekBalanceURL(baseURL string) string {
	return strings.TrimRight(strings.TrimSpace(baseURL), "/") + "/user/balance"
}

func fetchDeepSeek(ctx context.Context, client *http.Client, t Target) (*Result, error) {
	body, err := get(ctx, client, t, request{url: deepSeekBalanceURL(t.BaseURL), auth: "Bearer " + t.APIKey})
	if err != nil {
		if deepSeekEndpointMissing(err) {
			return nil, fmt.Errorf("%w: %v", ErrUnsupported, err)
		}
		return nil, err
	}
	infos := gjson.GetBytes(body, "balance_infos")
	if !infos.IsArray() {
		// A 2xx without balance_infos is a relay answering something else.
		return nil, fmt.Errorf("%w: response has no balance_infos", ErrUnsupported)
	}
	res := &Result{}
	infos.ForEach(func(_, info gjson.Result) bool {
		currency := strings.ToUpper(strings.TrimSpace(info.Get("currency").String()))
		if currency == "" {
			currency = "CNY"
		}
		total, ok := parseF64(info.Get("total_balance").Value())
		if !ok {
			return true
		}
		res.Balances = append(res.Balances, Balance{Kind: "total", Amount: total, Currency: currency})
		for _, k := range []struct{ field, kind string }{{"granted_balance", "granted"}, {"topped_up_balance", "topped_up"}} {
			if v, ok := parseF64(info.Get(k.field).Value()); ok {
				res.Balances = append(res.Balances, Balance{Kind: k.kind, Amount: v, Currency: currency})
			}
		}
		return true
	})
	if len(res.Balances) == 0 {
		return nil, fmt.Errorf("%w: no valid balance entries", ErrInvalidResponse)
	}
	available := true // absent means healthy, as in sub2api
	if v := gjson.GetBytes(body, "is_available"); v.Exists() {
		available = v.Bool()
	}
	res.BalanceAvailable = &available
	return res, nil
}

// deepSeekEndpointMissing reports whether a /user/balance error means the host
// has no such endpoint: relays answer 400 (treating it as a chat request),
// 404/405/501, or wrap a "not found" / "unsupported" upstream error.
func deepSeekEndpointMissing(err error) bool {
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.Business {
		return false
	}
	switch httpErr.Status {
	case http.StatusBadRequest, http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusNotImplemented:
		return true
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests:
		return false
	}
	body := strings.ToLower(httpErr.Body)
	return strings.Contains(body, "not found") || strings.Contains(body, "unsupported")
}
