// Parsers in this file are ported from sub2api (github.com/Wei-Shaw/sub2api,
// backend/internal/service/cn_provider_quota_service.go, commit 328db26),
// licensed under the GNU LGPL v3.0.

package provider

import (
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// Tier is one rolling usage window.
type Tier struct {
	Window      string  // "5h" | "weekly" | "monthly"
	UsedPercent float64 // 0-100+, not clamped
	ResetAt     string  // RFC3339, "" when the window has no reset time
	// Used / Limit are absolute amounts when the upstream reports them.
	Used  *float64
	Limit *float64
	Unit  string // unit of Used / Limit, e.g. "usd" or "count"
}

func f64(v float64) *float64 { return &v }

// parseKimiUsageTiers parses Kimi For Coding's /usages response.
//
// Two plan shapes coexist:
//   - legacy (5h + 7d): limits[].detail.{limit,remaining,resetTime} is the 5h
//     window (first valid detail), top-level usage.{...} is the weekly window.
//   - new (5h + monthly): usages.limit_5h / limit_7d / limit_month_total /
//     limit_month_code with used_ratio (0-1) + reset_time. Either monthly
//     window throttles, so monthly takes the larger usage and later reset.
//
// used_ratio wins over counts when both exist; usages.limit_5h only fills in
// when limits[] has no valid detail.
func parseKimiUsageTiers(body []byte) []Tier {
	var tiers []Tier

	hasFiveHour := false
	if limits := gjson.GetBytes(body, "limits"); limits.IsArray() {
		limits.ForEach(func(_, item gjson.Result) bool {
			detail := item.Get("detail")
			if !detail.Exists() {
				return true
			}
			limit, okLimit := parseF64(detail.Get("limit").Value())
			remaining, okRemaining := parseF64(detail.Get("remaining").Value())
			if !okLimit || !okRemaining || limit <= 0 || remaining < 0 || remaining > limit {
				return true
			}
			tiers = append(tiers, Tier{
				Window:      "5h",
				UsedPercent: (limit - remaining) / limit * 100,
				ResetAt:     normalizeResetTime(detail.Get("resetTime").Value()),
				Used:        f64(limit - remaining),
				Limit:       f64(limit),
				Unit:        "count",
			})
			hasFiveHour = true
			return false
		})
	}
	if !hasFiveHour {
		if tier, ok := kimiUsedRatioTier(body, "limit_5h", "5h"); ok {
			tiers = append(tiers, tier)
		}
	}

	if tier, ok := kimiUsedRatioTier(body, "limit_7d", "weekly"); ok {
		tiers = append(tiers, tier)
	} else if usage := gjson.GetBytes(body, "usage"); usage.Exists() {
		limit, okLimit := parseF64(usage.Get("limit").Value())
		remaining, okRemaining := parseF64(usage.Get("remaining").Value())
		if okLimit && okRemaining && limit > 0 && remaining >= 0 && remaining <= limit {
			tiers = append(tiers, Tier{
				Window:      "weekly",
				UsedPercent: (limit - remaining) / limit * 100,
				ResetAt:     normalizeResetTime(usage.Get("resetTime").Value()),
				Used:        f64(limit - remaining),
				Limit:       f64(limit),
				Unit:        "count",
			})
		}
	}

	// total and code are independent monthly limits; report the larger usage
	// and the later reset (the account stays throttled until both reset).
	total, hasTotal := kimiUsedRatioTier(body, "limit_month_total", "monthly")
	code, hasCode := kimiUsedRatioTier(body, "limit_month_code", "monthly")
	if hasTotal || hasCode {
		monthly := total
		if !hasTotal {
			monthly = code
		} else if hasCode {
			if code.UsedPercent > monthly.UsedPercent {
				monthly.UsedPercent = code.UsedPercent
			}
			if code.ResetAt != "" && (monthly.ResetAt == "" || code.ResetAt > monthly.ResetAt) {
				monthly.ResetAt = code.ResetAt
			}
		}
		tiers = append(tiers, monthly)
	}
	return tiers
}

// kimiUsedRatioTier parses usages.<key>.{used_ratio,reset_time}. used_ratio
// must be within [0,1].
func kimiUsedRatioTier(body []byte, key, window string) (Tier, bool) {
	node := gjson.GetBytes(body, "usages."+key)
	if !node.Exists() {
		return Tier{}, false
	}
	ratioNode := node.Get("used_ratio")
	if !ratioNode.Exists() {
		return Tier{}, false
	}
	ratio, ok := parseF64(ratioNode.Value())
	if !ok || ratio < 0 || ratio > 1 {
		return Tier{}, false
	}
	return Tier{
		Window:      window,
		UsedPercent: ratio * 100,
		ResetAt:     normalizeResetTime(node.Get("reset_time").Value()),
	}, true
}

// parseMiniMaxUsageTiers parses MiniMax Token Plan / Coding Plan remains.
// Only model_name == "general" counts; fields are remaining percentages.
func parseMiniMaxUsageTiers(body []byte) []Tier {
	remains := gjson.GetBytes(body, "model_remains")
	if !remains.IsArray() {
		return nil
	}
	var general gjson.Result
	remains.ForEach(func(_, item gjson.Result) bool {
		if strings.EqualFold(strings.TrimSpace(item.Get("model_name").String()), "general") {
			general = item
			return false
		}
		return true
	})
	if !general.Exists() {
		return nil
	}

	var tiers []Tier
	if remaining, ok := parseF64(general.Get("current_interval_remaining_percent").Value()); ok {
		tiers = append(tiers, Tier{
			Window:      "5h",
			UsedPercent: math.Max(0, 100-remaining),
			ResetAt:     minimaxResetTime(general.Get("end_time")),
		})
	}
	if general.Get("current_weekly_status").Int() == 1 {
		if remaining, ok := parseF64(general.Get("current_weekly_remaining_percent").Value()); ok {
			tiers = append(tiers, Tier{
				Window:      "weekly",
				UsedPercent: math.Max(0, 100-remaining),
				ResetAt:     minimaxResetTime(general.Get("weekly_end_time")),
			})
		}
	}
	return tiers
}

func minimaxResetTime(v gjson.Result) string {
	if !v.Exists() {
		return ""
	}
	ms := v.Int()
	if ms <= 0 {
		return normalizeResetTime(v.Value())
	}
	if ms < 1_000_000_000_000 {
		return time.Unix(ms, 0).UTC().Format(time.RFC3339)
	}
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}

type zhipuWindow int

const (
	zhipuWindowUnknown zhipuWindow = iota
	zhipuWindow5h
	zhipuWindowWeekly
)

// classifyZhipuWindowUnit: unit 3 = 5h, 6 = weekly.
func classifyZhipuWindowUnit(unit int64) zhipuWindow {
	switch unit {
	case 3:
		return zhipuWindow5h
	case 6:
		return zhipuWindowWeekly
	}
	return zhipuWindowUnknown
}

// parseZhipuTokenTiers parses data.limits into 5h + weekly.
//
//  1. An explicit unit (3 = 5h, 6 = weekly) wins. Reset-time ordering cannot
//     replace it: near the end of a week the weekly window resets first.
//  2. Without a recognized unit, entries without nextResetTime go to 5h first,
//     the rest fill the open slots in reset order.
//
// TOKENS_LIMIT and CREDIT_LIMIT measure different things; CREDIT_LIMIT is only
// used when no TOKENS_LIMIT entry exists (and is then also classified by unit).
func parseZhipuTokenTiers(data gjson.Result) []Tier {
	type entry struct {
		resetMs    int64
		hasReset   bool
		percentage float64
		resetISO   string
		unit       int64
	}
	var (
		fiveHour     entry
		fiveHourSet  bool
		weekly       entry
		weeklySet    bool
		unclassified []entry
	)
	classify := func(unit int64, e entry) {
		switch classifyZhipuWindowUnit(unit) {
		case zhipuWindow5h:
			if !fiveHourSet {
				fiveHour, fiveHourSet = e, true
			} else {
				unclassified = append(unclassified, e)
			}
		case zhipuWindowWeekly:
			if !weeklySet {
				weekly, weeklySet = e, true
			} else {
				unclassified = append(unclassified, e)
			}
		default:
			unclassified = append(unclassified, e)
		}
	}
	var creditFallback []entry
	hasTokensLimit := false

	data.Get("limits").ForEach(func(_, item gjson.Result) bool {
		limitType := strings.ToUpper(strings.TrimSpace(item.Get("type").String()))
		if limitType != "TOKENS_LIMIT" && limitType != "CREDIT_LIMIT" {
			return true
		}
		percentage, ok := parseF64(item.Get("percentage").Value())
		if !ok || percentage < 0 || percentage > 100 {
			return true
		}
		var (
			resetMs  int64
			hasReset bool
			resetISO string
		)
		if nr := item.Get("nextResetTime"); nr.Exists() {
			switch nr.Type {
			case gjson.Number:
				resetMs = nr.Int()
				hasReset = resetMs > 0
				resetISO = millisToRFC3339(resetMs)
			case gjson.String:
				resetISO = normalizeResetTime(nr.String())
				hasReset = resetISO != ""
			}
		}
		e := entry{resetMs: resetMs, hasReset: hasReset, percentage: percentage, resetISO: resetISO, unit: item.Get("unit").Int()}
		if limitType == "TOKENS_LIMIT" {
			hasTokensLimit = true
			classify(e.unit, e)
		} else {
			creditFallback = append(creditFallback, e)
		}
		return true
	})

	if !hasTokensLimit {
		for _, e := range creditFallback {
			if classifyZhipuWindowUnit(e.unit) != zhipuWindowUnknown {
				classify(e.unit, e)
				continue
			}
			unclassified = append(unclassified, e)
		}
	}

	sort.SliceStable(unclassified, func(i, j int) bool {
		if unclassified[i].hasReset != unclassified[j].hasReset {
			return !unclassified[i].hasReset
		}
		return unclassified[i].resetMs < unclassified[j].resetMs
	})
	for _, e := range unclassified {
		switch {
		case !fiveHourSet:
			fiveHour, fiveHourSet = e, true
		case !weeklySet:
			weekly, weeklySet = e, true
		}
	}

	var tiers []Tier
	if fiveHourSet {
		tiers = append(tiers, Tier{Window: "5h", UsedPercent: fiveHour.percentage, ResetAt: fiveHour.resetISO})
	}
	if weeklySet {
		tiers = append(tiers, Tier{Window: "weekly", UsedPercent: weekly.percentage, ResetAt: weekly.resetISO})
	}
	return tiers
}

// parseOpenCodeGoUsageTiers parses OpenCode Go GET /usage:
//
//	{"usage": {"rolling": {percent, resetsAt}, "weekly": {...}, "monthly": {...}}}
//
// The windows may also sit at the top level. rolling maps to 5h.
func parseOpenCodeGoUsageTiers(body []byte) []Tier {
	usage := gjson.GetBytes(body, "usage")
	if !usage.Exists() {
		usage = gjson.ParseBytes(body)
	}
	if !usage.IsObject() {
		return nil
	}
	var tiers []Tier
	for _, item := range []struct{ key, window string }{
		{"rolling", "5h"}, {"weekly", "weekly"}, {"monthly", "monthly"},
	} {
		node := usage.Get(item.key)
		if !node.Exists() {
			continue
		}
		percentNode := node.Get("percent")
		if !percentNode.Exists() {
			continue
		}
		used, ok := parseF64(percentNode.Value())
		if !ok {
			continue
		}
		tiers = append(tiers, Tier{
			Window:      item.window,
			UsedPercent: math.Max(0, used),
			ResetAt:     normalizeResetTime(node.Get("resetsAt").Value()),
		})
	}
	return tiers
}

// parseF64 parses a JSON number or numeric string; NaN and Inf are rejected.
func parseF64(raw any) (float64, bool) {
	var value float64
	switch v := raw.(type) {
	case float64:
		value = v
	case float32:
		value = float64(v)
	case int:
		value = float64(v)
	case int64:
		value = float64(v)
	case json.Number:
		f, err := v.Float64()
		if err != nil {
			return 0, false
		}
		value = f
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return 0, false
		}
		value = f
	default:
		return 0, false
	}
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, false
	}
	return value, true
}

var resetTimeFormats = []string{
	time.RFC3339,
	time.RFC3339Nano,
	"2006-01-02T15:04:05Z",
	"2006-01-02T15:04:05.000Z",
}

// normalizeResetTime converts an ISO 8601 string or a seconds / milliseconds
// timestamp into RFC3339; unrecognized values return "".
func normalizeResetTime(raw any) string {
	switch v := raw.(type) {
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return ""
		}
		for _, format := range resetTimeFormats {
			if ts, err := time.Parse(format, s); err == nil {
				return ts.UTC().Format(time.RFC3339)
			}
		}
		return ""
	case float64:
		return millisToRFC3339(int64(v))
	case int:
		return millisToRFC3339(int64(v))
	case int64:
		return millisToRFC3339(v)
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return millisToRFC3339(n)
		}
	}
	return ""
}

// millisToRFC3339 accepts seconds (< 1e12) or milliseconds; non-positive -> "".
func millisToRFC3339(n int64) string {
	if n <= 0 {
		return ""
	}
	ms := n
	if n < 1_000_000_000_000 {
		ms = n * 1000
	}
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}

// zhipuQuotaURL derives the GLM quota endpoint from the inference base URL.
func zhipuQuotaURL(baseURL string) string {
	return zhipuQuotaHost(baseURL) + "/api/monitor/usage/quota/limit"
}

func zhipuQuotaHost(baseURL string) string {
	switch u := strings.ToLower(baseURL); {
	case strings.Contains(u, "bigmodel.cn"):
		return "https://open.bigmodel.cn"
	case strings.Contains(u, "z.ai"):
		return "https://api.z.ai"
	default:
		return "https://open.bigmodel.cn"
	}
}

// kimiQuotaURL: both coding/v1 and coding bases map to /coding/v1/usages.
func kimiQuotaURL(baseURL string) string {
	base := strings.TrimSuffix(strings.TrimRight(baseURL, "/"), "/v1")
	return base + "/v1/usages"
}

// minimaxQuotaURL picks the international or mainland quota host.
func minimaxQuotaURL(baseURL string) string {
	if strings.Contains(strings.ToLower(baseURL), "minimax.io") {
		return "https://api.minimax.io/v1/api/openplatform/coding_plan/remains"
	}
	return "https://api.minimaxi.com/v1/api/openplatform/coding_plan/remains"
}
