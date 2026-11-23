package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"sort"
	"strings"

	"github.com/KexiChanProjectAI/llm-usage-exporter/internal/config"
	"github.com/KexiChanProjectAI/llm-usage-exporter/internal/source"
)

// Target is everything needed to fetch one quota: which provider endpoint to
// call and with which credentials. Accounts that resolve to the same Target
// share one fetch.
type Target struct {
	Provider string
	APIKey   string
	// BaseURL is the account's effective inference base URL; endpoint hosts are
	// derived from it the same way sub2api does.
	BaseURL         string
	ZhipuOrg        string
	ZhipuProject    string
	HeaderOverrides map[string]string
}

// Identity is a stable hash over every input that changes the upstream
// request. It never contains the key itself.
func (t Target) Identity() string {
	h := sha256.New()
	write := func(s string) { h.Write([]byte(s)); h.Write([]byte{0}) }
	write(t.Provider)
	write(t.APIKey)
	write(t.endpointKey())
	write(t.ZhipuOrg)
	write(t.ZhipuProject)
	names := make([]string, 0, len(t.HeaderOverrides))
	for k := range t.HeaderOverrides {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		write(k)
		write(t.HeaderOverrides[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// KeyHash identifies the API key for the auth blacklist: a new key in sub2api
// yields a new hash, which lifts the blacklist.
func (t Target) KeyHash() string {
	sum := sha256.Sum256([]byte(t.Provider + "\x00" + t.APIKey))
	return hex.EncodeToString(sum[:])
}

// endpointKey is the part of BaseURL that selects the endpoint.
func (t Target) endpointKey() string {
	switch t.Provider {
	case config.ProviderZhipu:
		return zhipuQuotaHost(t.BaseURL)
	case config.ProviderKimi:
		return kimiQuotaURL(t.BaseURL)
	case config.ProviderMiniMax:
		return minimaxQuotaURL(t.BaseURL)
	case config.ProviderDeepSeek:
		return deepSeekBalanceURL(t.BaseURL)
	default: // fixed official hosts
		return ""
	}
}

// Classify maps a sub2api account to a quota Target, porting sub2api's
// Account.GetCodingPlanProvider and IsOpenCodeGoUsageAccount. Only official
// hosts are accepted so that a key configured for a third-party relay is never
// sent to a vendor's quota endpoint.
func Classify(acc *source.Account) (Target, bool) {
	if acc == nil || acc.Credentials == nil {
		return Target{}, false
	}
	apiKey := strings.TrimSpace(acc.Credential("api_key"))
	if apiKey == "" {
		return Target{}, false
	}
	t := Target{APIKey: apiKey, HeaderOverrides: headerOverrides(acc)}
	baseURL := effectiveBaseURL(acc)
	t.BaseURL = baseURL

	switch acc.Platform {
	case config.ProviderOpenCodeGo:
		// Platform + mode are authoritative; Zen (pay-as-you-go) has no window.
		if strings.TrimSpace(acc.Credential("account_mode")) == "zen" {
			return Target{}, false
		}
		t.Provider = config.ProviderOpenCodeGo
		return t, true
	case config.ProviderCommandCode:
		if acc.Type == "apikey" && isHTTPSHost(baseURL, "api.commandcode.ai") {
			t.Provider = config.ProviderCommandCode
			return t, true
		}
		return Target{}, false
	case config.ProviderCline:
		if acc.Type == "apikey" && isHTTPSHost(baseURL, "api.cline.bot") {
			t.Provider = config.ProviderCline
			return t, true
		}
		return Target{}, false
	}

	// sub2api: IsCNProvider() && IsCodingPlan(), then the provider is picked by
	// base_url host regardless of which CN platform the account sits on.
	if isCNPlatform(acc.Platform) && accountMode(acc) == "coding" {
		lower := strings.ToLower(baseURL)
		switch {
		case isKimiCodingBaseURL(baseURL):
			t.Provider = config.ProviderKimi
		case strings.Contains(lower, "bigmodel.cn") || strings.Contains(lower, "api.z.ai"):
			t.Provider = config.ProviderZhipu
			t.ZhipuOrg = strings.TrimSpace(acc.Credential("zhipu_organization"))
			if t.ZhipuOrg != "" {
				t.ZhipuProject = strings.TrimSpace(acc.Credential("zhipu_project"))
			}
		case strings.Contains(lower, "minimax.io") || strings.Contains(lower, "minimaxi.com") ||
			strings.Contains(lower, "minimax.com"):
			t.Provider = config.ProviderMiniMax
		}
		if t.Provider != "" {
			return t, true
		}
	}

	// An OpenCode Go subscription key mounted on another platform.
	if acc.Type == "apikey" && isOpenCodeGoMountPlatform(acc.Platform) && isOpenCodeGoBaseURL(acc.Credential("base_url")) {
		t.Provider = config.ProviderOpenCodeGo
		return t, true
	}

	// DeepSeek pay-as-you-go balance, at <OpenAI-format base>/user/balance as
	// in sub2api. The request goes to the host the account already sends this
	// key to. For Anthropic-protocol accounts sub2api falls back to the
	// official base, which would send a relay's key to DeepSeek; those are
	// only queried when they are on the official host.
	if acc.Platform == config.ProviderDeepSeek && accountMode(acc) != "coding" {
		if strings.TrimSpace(acc.Credential("api_protocol")) == "anthropic" {
			if !isHTTPSHost(baseURL, "api.deepseek.com") {
				return Target{}, false
			}
			t.BaseURL = defaultBaseURLs["deepseek"].byMode["payg"]
		}
		if u, err := url.Parse(t.BaseURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return Target{}, false
		}
		t.Provider = config.ProviderDeepSeek
		return t, true
	}
	return Target{}, false
}

func isCNPlatform(p string) bool {
	switch p {
	case "kimi", "zhipu", "deepseek", "minimax":
		return true
	}
	return false
}

func isOpenCodeGoMountPlatform(p string) bool {
	switch p {
	case "openai", "anthropic", "kimi", "zhipu", "deepseek", "minimax":
		return true
	}
	return false
}

// accountMode mirrors Account.GetAccountMode: only payg / coding are valid.
func accountMode(acc *source.Account) string {
	switch m := strings.TrimSpace(acc.Credential("account_mode")); m {
	case "payg", "coding":
		return m
	}
	return ""
}

// defaultBaseURLs holds sub2api's provider profile defaults for the
// chat_completions protocol, keyed by platform then account_mode. The first
// listed mode of each platform is its default mode.
var defaultBaseURLs = map[string]struct {
	defaultMode string
	byMode      map[string]string
}{
	"kimi": {"payg", map[string]string{
		"payg": "https://api.moonshot.cn/v1", "coding": "https://api.kimi.com/coding/v1"}},
	"zhipu": {"payg", map[string]string{
		"payg": "https://open.bigmodel.cn/api/paas/v4", "coding": "https://open.bigmodel.cn/api/coding/paas/v4"}},
	"deepseek": {"payg", map[string]string{"payg": "https://api.deepseek.com"}},
	"minimax": {"payg", map[string]string{
		"payg": "https://api.minimaxi.com/v1", "coding": "https://api.minimaxi.com/v1"}},
	"opencode_go": {"go", map[string]string{
		"go": "https://opencode.ai/zen/go/v1", "zen": "https://opencode.ai/zen/v1"}},
	"command_code": {"payg", map[string]string{"payg": "https://api.commandcode.ai/provider/v1"}},
	"cline":        {"payg", map[string]string{"payg": "https://api.cline.bot/api/v1"}},
}

// routesByModel are the aggregator platforms whose protocol defaults to adaptive.
func routesByModel(platform string) bool {
	return platform == "opencode_go" || platform == "command_code"
}

// isAdaptive mirrors Account.GetAPIProtocol() == adaptive.
func isAdaptive(acc *source.Account) bool {
	switch strings.TrimSpace(acc.Credential("api_protocol")) {
	case "adaptive":
		return true
	case "anthropic", "chat_completions":
		return false
	case "responses":
		// Supported (non-adaptive) everywhere except zhipu and cline, where
		// sub2api falls through to the default.
		if acc.Platform != "zhipu" && acc.Platform != "cline" {
			return false
		}
	}
	return routesByModel(acc.Platform)
}

// effectiveBaseURL ports Account.GetOpenAIBaseURL for multi-protocol API key
// providers.
func effectiveBaseURL(acc *source.Account) string {
	if isAdaptive(acc) {
		if m, ok := acc.Credentials["api_base_urls"].(map[string]any); ok {
			if s, ok := m["chat_completions"].(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	if acc.Type == "apikey" || acc.Type == "upstream" {
		if s := strings.TrimSpace(acc.Credential("base_url")); s != "" {
			return s
		}
	}
	if d, ok := defaultBaseURLs[acc.Platform]; ok {
		if u, ok := d.byMode[strings.TrimSpace(acc.Credential("account_mode"))]; ok {
			return u
		}
		return d.byMode[d.defaultMode]
	}
	return ""
}

func isHTTPSHost(raw, host string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	return err == nil && strings.EqualFold(u.Scheme, "https") && strings.EqualFold(u.Hostname(), host)
}

func isKimiCodingBaseURL(baseURL string) bool {
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Opaque != "" ||
		parsed.RawQuery != "" || parsed.ForceQuery || strings.Contains(baseURL, "#") {
		return false
	}
	switch strings.ToLower(parsed.Host) {
	case "api.kimi.com", "api.kimi.ai", "api.kimi.com:443", "api.kimi.ai:443":
	default:
		return false
	}
	switch parsed.EscapedPath() {
	case "/coding", "/coding/", "/coding/v1", "/coding/v1/":
		return true
	}
	return false
}

func isOpenCodeGoBaseURL(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.ContainsAny(raw, "?#") {
		return false
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Opaque != "" || !strings.EqualFold(parsed.Scheme, "https") || parsed.User != nil ||
		parsed.ForceQuery || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawFragment != "" {
		return false
	}
	hostname := strings.ToLower(parsed.Hostname())
	if hostname != "opencode.ai" {
		return false
	}
	authority := strings.ToLower(parsed.Host)
	if authority != hostname && authority != hostname+":443" {
		return false
	}
	if parsed.RawPath != "" {
		return false
	}
	path := strings.TrimSuffix(parsed.Path, "/")
	return strings.EqualFold(path, "/zen/go/v1") || strings.EqualFold(path, "/zen/go")
}

// headerOverrideBlocked mirrors sub2api's headerOverrideBlockedNames.
var headerOverrideBlocked = map[string]bool{
	"host": true, "content-length": true, "content-type": true, "transfer-encoding": true,
	"connection": true, "keep-alive": true, "proxy-authenticate": true, "proxy-authorization": true,
	"proxy-connection": true, "te": true, "trailer": true, "upgrade": true, "authorization": true,
	"x-api-key": true, "x-goog-api-key": true, "cookie": true, "accept-encoding": true,
	"sec-websocket-key": true, "sec-websocket-version": true, "sec-websocket-extensions": true,
	"sec-websocket-protocol": true, "sec-websocket-accept": true, "session_id": true,
	"conversation_id": true, "x-codex-turn-state": true, "x-codex-turn-metadata": true,
	"chatgpt-account-id": true, "x-claude-code-session-id": true, "x-client-request-id": true,
	"x-grok-conv-id": true,
}

// headerOverrides returns the account's enabled header overrides (lowercase
// names). sub2api applies them to its quota probes too, so we do the same.
func headerOverrides(acc *source.Account) map[string]string {
	if acc.Type != "apikey" {
		return nil
	}
	if enabled, _ := acc.Credentials["header_override_enabled"].(bool); !enabled {
		return nil
	}
	raw, ok := acc.Credentials["header_overrides"].(map[string]any)
	if !ok {
		return nil
	}
	out := map[string]string{}
	for name, v := range raw {
		value, ok := v.(string)
		name = strings.ToLower(strings.TrimSpace(name))
		value = strings.TrimSpace(value)
		if !ok || name == "" || value == "" || headerOverrideBlocked[name] || strings.ContainsAny(value, "\r\n") {
			continue
		}
		out[name] = value
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
