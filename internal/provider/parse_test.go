// Tests in this file are ported from sub2api (github.com/Wei-Shaw/sub2api,
// backend/internal/service/cn_providers_test.go, commit 328db26),
// licensed under the GNU LGPL v3.0.

package provider

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// - 仅取首个 detail（多个 detail 时不应重复产出 5h）
func TestParseKimiUsageTiers(t *testing.T) {
	t.Parallel()
	body := []byte(`{
		"limits": [
			{"name": "5h", "detail": {"limit": 1000, "remaining": 600, "resetTime": "2026-08-14T15:00:00Z"}},
			{"name": "ignored-second-detail", "detail": {"limit": 999, "remaining": 0, "resetTime": "2026-08-14T20:00:00Z"}}
		],
		"usage": {"limit": 10000, "remaining": 4000, "resetTime": "2026-08-18T00:00:00Z"}
	}`)
	tiers := parseKimiUsageTiers(body)
	require.Len(t, tiers, 2)
	require.Equal(t, "5h", tiers[0].Window)
	require.InDelta(t, 40.0, tiers[0].UsedPercent, 1e-9) // (1000-600)/1000*100
	require.Equal(t, "2026-08-14T15:00:00Z", tiers[0].ResetAt)
	require.Equal(t, "weekly", tiers[1].Window)
	require.InDelta(t, 60.0, tiers[1].UsedPercent, 1e-9) // (10000-4000)/10000*100
	require.Equal(t, "2026-08-18T00:00:00Z", tiers[1].ResetAt)
}

// TestParseKimiUsageTiers_LimitZero 不应除零：limit=0 → utilization=0。
func TestParseKimiUsageTiers_LimitZero(t *testing.T) {
	t.Parallel()
	body := []byte(`{"limits":[{"detail":{"limit":0,"remaining":0,"resetTime":"2026-08-14T15:00:00Z"}}]}`)
	tiers := parseKimiUsageTiers(body)
	// limit=0 无法计算利用率：不能产出「已用 0%」的伪窗口（那会把额度耗尽
	// 显示成完全没用），该条目应被跳过。
	require.Len(t, tiers, 0)
}

// TestParseKimiUsageTiers_MonthlyPlan 新套餐形态（5h + 月度限额）：不再有顶层
// usage，改由 usages.limit_5h / limit_month_total / limit_month_code 以
// used_ratio（0-1）+ reset_time 上报。monthly 取两条月度键的较大用量 + 较早重置。
func TestParseKimiUsageTiers_MonthlyPlan(t *testing.T) {
	t.Parallel()
	body := []byte(`{
		"limits": [{
			"window": {"duration": 300, "timeUnit": "TIME_UNIT_MINUTE"},
			"detail": {"limit": "100", "used": "10", "remaining": "90", "resetTime": "2026-11-13T16:10:29.523075Z"}
		}],
		"usages": {
			"limit_5h": {"used_ratio": 0.0982, "reset_time": "2026-11-13T16:10:28Z"},
			"limit_month_total": {"used_ratio": 0.6564, "reset_time": "2026-11-26T00:00:00Z"},
			"limit_month_code": {"used_ratio": 0.6252, "reset_time": "2026-11-26T00:00:00Z"}
		}
	}`)
	tiers := parseKimiUsageTiers(body)
	require.Len(t, tiers, 2, "新套餐应为 5h + monthly，且不得产出已废弃的 weekly")

	require.Equal(t, "5h", tiers[0].Window)
	require.InDelta(t, 10.0, tiers[0].UsedPercent, 1e-9, "5h 用 limits[].detail 的绝对计数")
	require.Equal(t, "2026-11-13T16:10:29Z", tiers[0].ResetAt)

	require.Equal(t, "monthly", tiers[1].Window)
	require.InDelta(t, 65.64, tiers[1].UsedPercent, 1e-9, "used_ratio 0-1 换算为百分比")
	require.Equal(t, "2026-11-26T00:00:00Z", tiers[1].ResetAt)
}

// TestParseKimiUsageTiers_MonthlyPicksMaxUsage total/code 任一耗尽都会限流：
// monthly 取较大用量；重置时间取较早者（先恢复额度，避免过度停调）。
func TestParseKimiUsageTiers_MonthlyPicksMaxUsage(t *testing.T) {
	t.Parallel()
	body := []byte(`{"usages":{
		"limit_month_total":{"used_ratio":0.4,"reset_time":"2026-11-26T00:00:00Z"},
		"limit_month_code":{"used_ratio":0.9,"reset_time":"2026-11-20T00:00:00Z"}}}`)
	tiers := parseKimiUsageTiers(body)
	require.Len(t, tiers, 1)
	require.Equal(t, "monthly", tiers[0].Window)
	require.InDelta(t, 90.0, tiers[0].UsedPercent, 1e-9)
	// total 与 code 是两个独立限流窗口：reset 必须取较晚者，否则 code 先到期的
	// 11-20 会让仍处于 40% 的 total 窗口提前放行。
	require.Equal(t, "2026-11-26T00:00:00Z", tiers[0].ResetAt)

	// 两窗口同为 100% 时也不得取更早的 reset。
	bothFull := parseKimiUsageTiers([]byte(`{"usages":{
		"limit_month_total":{"used_ratio":1,"reset_time":"2026-11-26T00:00:00Z"},
		"limit_month_code":{"used_ratio":1,"reset_time":"2026-11-20T00:00:00Z"}}}`))
	require.Len(t, bothFull, 1)
	require.Equal(t, "2026-11-26T00:00:00Z", bothFull[0].ResetAt)

	onlyCode := parseKimiUsageTiers([]byte(`{"usages":{"limit_month_code":{"used_ratio":0.5,"reset_time":"2026-11-20T00:00:00Z"}}}`))
	require.Len(t, onlyCode, 1)
	require.Equal(t, "monthly", onlyCode[0].Window)
	require.InDelta(t, 50.0, onlyCode[0].UsedPercent, 1e-9)
}

// TestParseKimiUsageTiers_LegacyWithUsedRatio 老套餐（顶层 usage）同时携带
// usages.limit_7d 时，weekly 以 used_ratio 为准、不重复产出 weekly 档。
func TestParseKimiUsageTiers_LegacyWithUsedRatio(t *testing.T) {
	t.Parallel()
	body := []byte(`{
		"usage": {"limit": "100", "remaining": "80", "resetTime": "2026-10-07T05:04:39.713183Z"},
		"usages": {
			"limit_5h": {"used_ratio": 0, "reset_time": "2026-10-04T11:04:38Z"},
			"limit_7d": {"used_ratio": 0.203912, "reset_time": "2026-10-07T05:04:38Z"}
		}
	}`)
	tiers := parseKimiUsageTiers(body)
	require.Len(t, tiers, 2)
	require.Equal(t, "5h", tiers[0].Window)
	require.Equal(t, "weekly", tiers[1].Window)
	require.InDelta(t, 20.3912, tiers[1].UsedPercent, 1e-9)
	require.Equal(t, "2026-10-07T05:04:38Z", tiers[1].ResetAt)
}

// TestParseZhipuTokenTiers_UnitClassification 显式 unit（3=5h / 6=weekly）优先分类，
// 不能被 reset 时间排序覆盖（周期末尾周窗口会更早重置）。
func TestParseZhipuTokenTiers_UnitClassification(t *testing.T) {
	t.Parallel()
	// weekly 的 nextResetTime 早于 5h（模拟周期末尾），但 unit 必须胜出。
	data := gjson.Parse(`{
		"limits": [
			{"type":"TOKENS_LIMIT","unit":6,"percentage":70,"nextResetTime":1700000000000},
			{"type":"TOKENS_LIMIT","unit":3,"percentage":20,"nextResetTime":1700000099999}
		]
	}`)
	tiers := parseZhipuTokenTiers(data)
	require.Len(t, tiers, 2)
	require.Equal(t, "5h", tiers[0].Window)
	require.InDelta(t, 20.0, tiers[0].UsedPercent, 1e-9)
	require.Equal(t, "weekly", tiers[1].Window)
	require.InDelta(t, 70.0, tiers[1].UsedPercent, 1e-9)
}

// TestParseZhipuTokenTiers_CreditLimitUnitClassification GLM Coding Plan 当前把 5h / weekly
// 两条窗口都标成 CREDIT_LIMIT（实测 level=lite）。CREDIT_LIMIT 同样携带 unit，
// 必须和 TOKENS_LIMIT 一样按 unit 显式分类，否则会退化成 reset 时间排序 ——
// 此处刻意让 weekly 的 nextResetTime 早于 5h（周期末尾），两档会被标反。
func TestParseZhipuTokenTiers_CreditLimitUnitClassification(t *testing.T) {
	t.Parallel()
	data := gjson.Parse(`{
		"limits": [
			{"type":"CREDIT_LIMIT","unit":3,"number":5,"percentage":14,"nextResetTime":1700000099999},
			{"type":"CREDIT_LIMIT","unit":6,"number":1,"percentage":90,"nextResetTime":1700000000000}
		]
	}`)
	tiers := parseZhipuTokenTiers(data)
	require.Len(t, tiers, 2)
	require.Equal(t, "5h", tiers[0].Window)
	require.InDelta(t, 14.0, tiers[0].UsedPercent, 1e-9)
	require.Equal(t, "weekly", tiers[1].Window)
	require.InDelta(t, 90.0, tiers[1].UsedPercent, 1e-9)
}

// TestParseZhipuTokenTiers_CreditLimitWithoutUnitFallback CREDIT_LIMIT 缺 unit 时，
// 保留原有 fallback：无 nextResetTime 的优先归 5h，其余按 reset 升序填入剩余槽位。
func TestParseZhipuTokenTiers_CreditLimitWithoutUnitFallback(t *testing.T) {
	t.Parallel()
	data := gjson.Parse(`{
		"limits": [
			{"type":"CREDIT_LIMIT","percentage":30,"nextResetTime":1700000000000},
			{"type":"CREDIT_LIMIT","percentage":80}
		]
	}`)
	tiers := parseZhipuTokenTiers(data)
	require.Len(t, tiers, 2)
	require.Equal(t, "5h", tiers[0].Window)
	require.InDelta(t, 80.0, tiers[0].UsedPercent, 1e-9)
	require.Equal(t, "weekly", tiers[1].Window)
	require.InDelta(t, 30.0, tiers[1].UsedPercent, 1e-9)
}

// TestParseZhipuTokenTiers_TokensLimitWinsOverCreditLimit 两种 type 同时返回时，
// TOKENS_LIMIT 仍然优先占槽、CREDIT_LIMIT 不参与（原有语义保持不变）。
func TestParseZhipuTokenTiers_TokensLimitWinsOverCreditLimit(t *testing.T) {
	t.Parallel()
	data := gjson.Parse(`{
		"limits": [
			{"type":"TOKENS_LIMIT","unit":3,"percentage":20,"nextResetTime":1700000000000},
			{"type":"CREDIT_LIMIT","unit":6,"percentage":99,"nextResetTime":1700000099999}
		]
	}`)
	tiers := parseZhipuTokenTiers(data)
	require.Len(t, tiers, 1)
	require.Equal(t, "5h", tiers[0].Window)
	require.InDelta(t, 20.0, tiers[0].UsedPercent, 1e-9)
}

// TestParseZhipuTokenTiers_SingleTierOldPlan 老套餐仅回 1 条 → 降级为仅 5h。
func TestParseZhipuTokenTiers_SingleTierOldPlan(t *testing.T) {
	t.Parallel()
	data := gjson.Parse(`{"limits":[{"type":"TOKENS_LIMIT","unit":3,"percentage":15,"nextResetTime":1700000000000}]}`)
	tiers := parseZhipuTokenTiers(data)
	require.Len(t, tiers, 1)
	require.Equal(t, "5h", tiers[0].Window)
}

// TestParseZhipuTokenTiers_FallbackHeuristic unit 缺失时：无 reset 的条目优先归 5h，
// 其余按 reset 升序填入剩余槽位。
func TestParseZhipuTokenTiers_FallbackHeuristic(t *testing.T) {
	t.Parallel()
	// 无 unit：A 无 reset、B 有 reset。A 先填 5h，B 填 weekly。
	data := gjson.Parse(`{
		"limits": [
			{"type":"TOKENS_LIMIT","percentage":50,"nextResetTime":1700000000000},
			{"type":"TOKENS_LIMIT","percentage":10}
		]
	}`)
	tiers := parseZhipuTokenTiers(data)
	require.Len(t, tiers, 2)
	require.Equal(t, "5h", tiers[0].Window)
	require.InDelta(t, 10.0, tiers[0].UsedPercent, 1e-9) // 无 reset 优先 5h
	require.Equal(t, "weekly", tiers[1].Window)
	require.InDelta(t, 50.0, tiers[1].UsedPercent, 1e-9)
}

// TestParseZhipuTokenTiers_IgnoresNonTokenEntries 非 TOKENS_LIMIT/CREDIT_LIMIT 条目跳过。
func TestParseZhipuTokenTiers_IgnoresNonTokenEntries(t *testing.T) {
	t.Parallel()
	data := gjson.Parse(`{"limits":[{"type":"OTHER_LIMIT","unit":3,"percentage":99}]}`)
	require.Empty(t, parseZhipuTokenTiers(data))
}

// TestParseKimiUsageTiers_RejectsMalformedWindows 受损/改版结构不得产出伪窗口。
// 这类窗口一旦被当成有效快照落库，缺席清理会把其他窗口写成 nil，
// 调度层看到「没有窗口」就会解除预停调。
func TestParseKimiUsageTiers_RejectsMalformedWindows(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		body []byte
	}{
		"empty object":          {[]byte(`{}`)},
		"empty detail":          {[]byte(`{"limits":[{"detail":{}}]}`)},
		"limit zero":            {[]byte(`{"limits":[{"detail":{"limit":0,"remaining":0}}]}`)},
		"remaining exceeds":     {[]byte(`{"limits":[{"detail":{"limit":100,"remaining":200}}]}`)},
		"limit not a number":    {[]byte(`{"limits":[{"detail":{"limit":"abc","remaining":"1"}}]}`)},
		"empty usage":           {[]byte(`{"usage":{}}`)},
		"ratio out of range":    {[]byte(`{"usages":{"limit_5h":{"used_ratio":65}}}`)},
		"ratio NaN":             {[]byte(`{"usages":{"limit_5h":{"used_ratio":"NaN"}}}`)},
		"ratio negative":        {[]byte(`{"usages":{"limit_5h":{"used_ratio":-0.2}}}`)},
		"renamed top level key": {[]byte(`{"limits_v2":[{"detail":{"limit":100,"remaining":50}}]}`)},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tiers := parseKimiUsageTiers(tc.body)
			require.Empty(t, tiers, "受损结构不得产出窗口：%s", string(tc.body))
		})
	}
}

// TestParseKimiUsageTiers_KeepsLegalEdgeValues 合法边界（0%、满额、单窗口、整数）不得被误伤。
func TestParseKimiUsageTiers_KeepsLegalEdgeValues(t *testing.T) {
	t.Parallel()
	// remaining == limit → 合法的 0%。
	tiers := parseKimiUsageTiers([]byte(`{"limits":[{"detail":{"limit":100,"remaining":100}}]}`))
	require.Len(t, tiers, 1)
	require.InDelta(t, 0.0, tiers[0].UsedPercent, 1e-9)

	// remaining == 0 → 100%。
	tiers = parseKimiUsageTiers([]byte(`{"limits":[{"detail":{"limit":100,"remaining":0}}]}`))
	require.Len(t, tiers, 1)
	require.InDelta(t, 100.0, tiers[0].UsedPercent, 1e-9)

	// used_ratio == 0 / 1 同样合法。
	tiers = parseKimiUsageTiers([]byte(`{"usages":{"limit_5h":{"used_ratio":0}}}`))
	require.Len(t, tiers, 1)
	require.InDelta(t, 0.0, tiers[0].UsedPercent, 1e-9)

	tiers = parseKimiUsageTiers([]byte(`{"usages":{"limit_5h":{"used_ratio":1}}}`))
	require.Len(t, tiers, 1)
	require.InDelta(t, 100.0, tiers[0].UsedPercent, 1e-9)

	// 首个 detail 坏、后续合法 → 采用首个有效条目。
	tiers = parseKimiUsageTiers([]byte(`{"limits":[{"detail":{}},{"detail":{"limit":100,"remaining":60}}]}`))
	require.Len(t, tiers, 1)
	require.InDelta(t, 40.0, tiers[0].UsedPercent, 1e-9)
}

// TestParseZhipuTokenTiers_SkipsMissingPercentage percentage 缺失/非法不得伪造 0% 窗口。
func TestParseZhipuTokenTiers_SkipsMissingPercentage(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		body      string
		wantTiers int
	}{
		"missing percentage": {`{"limits":[{"type":"TOKENS_LIMIT","unit":3}]}`, 0},
		"non numeric":        {`{"limits":[{"type":"TOKENS_LIMIT","unit":3,"percentage":"abc"}]}`, 0},
		"negative":           {`{"limits":[{"type":"TOKENS_LIMIT","unit":3,"percentage":-5}]}`, 0},
		"over 100":           {`{"limits":[{"type":"TOKENS_LIMIT","unit":3,"percentage":120}]}`, 0},
		"legal zero":         {`{"limits":[{"type":"TOKENS_LIMIT","unit":3,"percentage":0}]}`, 1},
		"legal five hour":    {`{"limits":[{"type":"TOKENS_LIMIT","unit":3,"percentage":42}]}`, 1},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tiers := parseZhipuTokenTiers(gjson.Parse(tc.body))
			require.Len(t, tiers, tc.wantTiers, "输入：%s", tc.body)
		})
	}
}

func TestParseOpenCodeGoUsageTiers(t *testing.T) {
	t.Parallel()
	body := []byte(`{
		"usage": {
			"rolling": {"percent": 12.5, "resetsAt": "2026-09-07T12:00:00Z"},
			"weekly": {"percent": 40, "resetsAt": "2026-09-10T00:00:00Z"},
			"monthly": {"percent": 22.2, "resetsAt": "2026-10-01T00:00:00Z"}
		}
	}`)
	tiers := parseOpenCodeGoUsageTiers(body)
	require.Len(t, tiers, 3)
	require.Equal(t, "5h", tiers[0].Window)
	require.Equal(t, 12.5, tiers[0].UsedPercent)
	require.Equal(t, "2026-09-07T12:00:00Z", tiers[0].ResetAt)
	require.Equal(t, "weekly", tiers[1].Window)
	require.Equal(t, 40.0, tiers[1].UsedPercent)
	require.Equal(t, "monthly", tiers[2].Window)
	require.InDelta(t, 22.2, tiers[2].UsedPercent, 0.001)
	require.Empty(t, parseOpenCodeGoUsageTiers([]byte(`{"ok":true}`)))
}

// TestZhipuQuotaHost 按域名路由智谱额度端点主机（bigmodel.cn / z.ai / 默认国内站）。
func TestZhipuQuotaHost(t *testing.T) {
	t.Parallel()
	require.Equal(t, "https://open.bigmodel.cn", zhipuQuotaHost("https://open.bigmodel.cn/api/paas/v4"))
	require.Equal(t, "https://api.z.ai", zhipuQuotaHost("https://api.z.ai/api/paas/v4"))
	require.Equal(t, "https://open.bigmodel.cn", zhipuQuotaHost("https://custom.example.com")) // 默认国内站
	require.Equal(t, "https://open.bigmodel.cn/api/monitor/usage/quota/limit", zhipuQuotaURL("https://open.bigmodel.cn/api/paas/v4"))
}

// TestKimiQuotaURL 两种协议默认 base（coding/v1 与 coding）都归一到 /coding/v1/usages
// （cc-switch 固定端点；无 /v1 的 /coding/usages 实测 404）。
func TestKimiQuotaURL(t *testing.T) {
	t.Parallel()
	require.Equal(t, "https://api.kimi.com/coding/v1/usages", kimiQuotaURL("https://api.kimi.com/coding/v1"))
	require.Equal(t, "https://api.kimi.com/coding/v1/usages", kimiQuotaURL("https://api.kimi.com/coding"))
	require.Equal(t, "https://api.kimi.com/coding/v1/usages", kimiQuotaURL("https://api.kimi.com/coding/"))
	require.Equal(t, "https://api.kimi.com/coding/v1/usages", kimiQuotaURL("https://api.kimi.com/coding/v1/"))
}

func TestMiniMaxQuotaURL(t *testing.T) {
	t.Parallel()
	require.Equal(t, "https://api.minimax.io/v1/api/openplatform/coding_plan/remains",
		minimaxQuotaURL("https://api.minimax.io/v1"))
	require.Equal(t, "https://api.minimax.io/v1/api/openplatform/coding_plan/remains",
		minimaxQuotaURL("https://api.minimax.io/anthropic"))
	require.Equal(t, "https://api.minimaxi.com/v1/api/openplatform/coding_plan/remains",
		minimaxQuotaURL("https://api.minimaxi.com/v1"))
	require.Equal(t, "https://api.minimaxi.com/v1/api/openplatform/coding_plan/remains",
		minimaxQuotaURL("https://api.minimax.com/v1"))
	require.Equal(t, "https://api.minimaxi.com/v1/api/openplatform/coding_plan/remains",
		minimaxQuotaURL("https://custom.example.com"))
}

func TestParseMiniMaxUsageTiers(t *testing.T) {
	t.Parallel()
	endMs := int64(1_700_000_000_000)
	weeklyEndMs := endMs + 7*24*60*60*1000
	body := []byte(`{
		"base_resp": {"status_code": 0},
		"current_subscribe_title": "Max",
		"model_remains": [
			{"model_name": "video", "current_interval_remaining_percent": 10},
			{
				"model_name": "general",
				"current_interval_remaining_percent": 25,
				"end_time": 1700000000000,
				"current_weekly_status": 1,
				"current_weekly_remaining_percent": 40,
				"weekly_end_time": 1700604800000
			}
		]
	}`)
	tiers := parseMiniMaxUsageTiers(body)
	require.Len(t, tiers, 2)
	require.Equal(t, "5h", tiers[0].Window)
	require.InDelta(t, 75, tiers[0].UsedPercent, 1e-9)
	require.Equal(t, time.UnixMilli(endMs).UTC().Format(time.RFC3339), tiers[0].ResetAt)
	require.Equal(t, "weekly", tiers[1].Window)
	require.InDelta(t, 60, tiers[1].UsedPercent, 1e-9)
	require.Equal(t, time.UnixMilli(weeklyEndMs).UTC().Format(time.RFC3339), tiers[1].ResetAt)

	noWeekly := parseMiniMaxUsageTiers([]byte(`{
		"model_remains": [{
			"model_name": "general",
			"current_interval_remaining_percent": 80,
			"end_time": 1700000000000,
			"current_weekly_status": 3,
			"current_weekly_remaining_percent": 10
		}]
	}`))
	require.Len(t, noWeekly, 1)
	require.Equal(t, "5h", noWeekly[0].Window)
	require.InDelta(t, 20, noWeekly[0].UsedPercent, 1e-9)

	require.Nil(t, parseMiniMaxUsageTiers([]byte(`{"model_remains":[{"model_name":"video","current_interval_remaining_percent":5}]}`)))
}
