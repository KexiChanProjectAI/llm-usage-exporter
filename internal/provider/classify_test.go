package provider

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/KexiChanProjectAI/llm-usage-exporter/internal/config"
	"github.com/KexiChanProjectAI/llm-usage-exporter/internal/source"
)

func acct(platform, typ string, creds map[string]any) *source.Account {
	if creds != nil {
		if _, ok := creds["api_key"]; !ok {
			creds["api_key"] = "sk-test"
		}
	}
	return &source.Account{ID: 1, Name: "a", Platform: platform, Type: typ, Credentials: creds}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		acc  *source.Account
		want string // "" = not classified
	}{
		{"zhipu coding default base", acct("zhipu", "apikey", map[string]any{"account_mode": "coding"}), "zhipu"},
		{"zhipu payg", acct("zhipu", "apikey", map[string]any{"account_mode": "payg"}), ""},
		{"zhipu no mode", acct("zhipu", "apikey", map[string]any{}), ""},
		{"zhipu z.ai", acct("zhipu", "apikey", map[string]any{"account_mode": "coding", "base_url": "https://api.z.ai/api/anthropic"}), "zhipu"},
		{"zhipu relay", acct("zhipu", "apikey", map[string]any{"account_mode": "coding", "base_url": "https://relay.example.com/v1"}), ""},
		{"kimi coding default", acct("kimi", "apikey", map[string]any{"account_mode": "coding"}), "kimi"},
		{"kimi coding anthropic base", acct("kimi", "apikey", map[string]any{"account_mode": "coding", "base_url": "https://api.kimi.com/coding"}), "kimi"},
		{"kimi coding http rejected", acct("kimi", "apikey", map[string]any{"account_mode": "coding", "base_url": "http://api.kimi.com/coding"}), ""},
		{"kimi payg", acct("kimi", "apikey", map[string]any{"account_mode": "payg"}), ""},
		{"minimax coding", acct("minimax", "apikey", map[string]any{"account_mode": "coding"}), "minimax"},
		{"minimax intl", acct("minimax", "apikey", map[string]any{"account_mode": "coding", "base_url": "https://api.minimax.io/anthropic"}), "minimax"},
		{"deepseek coding is not a plan", acct("deepseek", "apikey", map[string]any{"account_mode": "coding"}), ""},
		{"deepseek payg default base", acct("deepseek", "apikey", map[string]any{"account_mode": "payg"}), "deepseek"},
		{"deepseek no mode", acct("deepseek", "apikey", map[string]any{}), "deepseek"},
		{"deepseek anthropic base", acct("deepseek", "apikey", map[string]any{"base_url": "https://api.deepseek.com/anthropic"}), "deepseek"},
		{"deepseek relay", acct("deepseek", "apikey", map[string]any{"base_url": "https://relay.example.com/v1"}), "deepseek"},
		{"deepseek relay anthropic", acct("deepseek", "apikey", map[string]any{"base_url": "https://relay.example.com", "api_protocol": "anthropic"}), ""},
		{"deepseek official anthropic", acct("deepseek", "apikey", map[string]any{"base_url": "https://api.deepseek.com/anthropic", "api_protocol": "anthropic"}), "deepseek"},
		{"deepseek mounted opencode", acct("deepseek", "apikey", map[string]any{"base_url": "https://opencode.ai/zen/go/v1"}), "opencode_go"},
		{"opencode go default", acct("opencode_go", "apikey", map[string]any{}), "opencode_go"},
		{"opencode go relay base still official key", acct("opencode_go", "apikey", map[string]any{"base_url": "https://relay.example.com"}), "opencode_go"},
		{"opencode zen", acct("opencode_go", "apikey", map[string]any{"account_mode": "zen"}), ""},
		{"opencode mounted on openai", acct("openai", "apikey", map[string]any{"base_url": "https://opencode.ai/zen/go/v1"}), "opencode_go"},
		{"opencode zen mounted", acct("openai", "apikey", map[string]any{"base_url": "https://opencode.ai/zen/v1"}), ""},
		{"opencode mounted upstream type", acct("openai", "upstream", map[string]any{"base_url": "https://opencode.ai/zen/go"}), ""},
		{"openai plain", acct("openai", "apikey", map[string]any{"base_url": "https://api.openai.com"}), ""},
		{"command code default", acct("command_code", "apikey", map[string]any{}), "command_code"},
		{"command code relay", acct("command_code", "apikey", map[string]any{"base_url": "https://relay.example.com/v1"}), ""},
		{"command code adaptive", acct("command_code", "apikey", map[string]any{
			"base_url": "https://relay.example.com/v1", "api_base_urls": map[string]any{"chat_completions": "https://api.commandcode.ai/provider/v1"}}), "command_code"},
		{"command code upstream type", acct("command_code", "upstream", map[string]any{}), ""},
		{"cline default", acct("cline", "apikey", map[string]any{}), "cline"},
		{"cline relay", acct("cline", "apikey", map[string]any{"base_url": "https://relay.example.com"}), ""},
		{"no key", acct("zhipu", "apikey", map[string]any{"account_mode": "coding", "api_key": "  "}), ""},
		{"no credentials", acct("zhipu", "apikey", nil), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Classify(tc.acc)
			if tc.want == "" {
				require.False(t, ok, "classified as %q", got.Provider)
				return
			}
			require.True(t, ok)
			require.Equal(t, tc.want, got.Provider)
		})
	}
}

func TestClassifyZhipuTeamAndOverrides(t *testing.T) {
	a := acct("zhipu", "apikey", map[string]any{
		"account_mode": "coding", "zhipu_organization": " org ", "zhipu_project": "p",
		"header_override_enabled": true,
		"header_overrides":        map[string]any{"X-Foo": "bar", "Authorization": "evil", "x-empty": ""},
	})
	got, ok := Classify(a)
	require.True(t, ok)
	require.Equal(t, "org", got.ZhipuOrg)
	require.Equal(t, "p", got.ZhipuProject)
	require.Equal(t, map[string]string{"x-foo": "bar"}, got.HeaderOverrides)
}

func TestTargetIdentityAndKeyHash(t *testing.T) {
	a := Target{Provider: "kimi", APIKey: "k1", BaseURL: "https://api.kimi.com/coding/v1"}
	b := Target{Provider: "kimi", APIKey: "k1", BaseURL: "https://api.kimi.com/coding"}
	require.Equal(t, a.Identity(), b.Identity(), "both bases hit the same endpoint")
	c := a
	c.APIKey = "k2"
	require.NotEqual(t, a.Identity(), c.Identity())
	require.NotEqual(t, a.KeyHash(), c.KeyHash())
	require.NotContains(t, a.Identity(), "k1")
}

func TestDeepSeekBalanceURL(t *testing.T) {
	cases := map[string]struct {
		creds map[string]any
		want  string
	}{
		"official default":   {map[string]any{}, "https://api.deepseek.com/user/balance"},
		"relay with v1":      {map[string]any{"base_url": "https://relay.example.com/v1/"}, "https://relay.example.com/v1/user/balance"},
		"adaptive chat base": {map[string]any{"base_url": "https://x.example.com", "api_protocol": "adaptive", "api_base_urls": map[string]any{"chat_completions": "https://y.example.com/v1"}}, "https://y.example.com/v1/user/balance"},
		"official anthropic": {map[string]any{"base_url": "https://api.deepseek.com/anthropic", "api_protocol": "anthropic"}, "https://api.deepseek.com/user/balance"},
	}
	for name, tc := range cases {
		got, ok := Classify(acct("deepseek", "apikey", tc.creds))
		require.True(t, ok, name)
		require.Equal(t, tc.want, deepSeekBalanceURL(got.BaseURL), name)
	}
}

func TestClassifyStaticAccounts(t *testing.T) {
	accs := source.StaticAccounts([]config.StaticAccount{
		{Name: "ds", Provider: "deepseek", APIKey: "sk"},
		{Name: "glm", Provider: "zhipu", APIKey: "k", AccountMode: "coding"},
		{Name: "kimi", Provider: "kimi", APIKey: "k", AccountMode: "coding"},
		{Name: "cc", Provider: "command_code", APIKey: "k"},
	})
	want := []string{"deepseek", "zhipu", "kimi", "command_code"}
	for i := range accs {
		got, ok := Classify(&accs[i])
		require.True(t, ok, accs[i].Name)
		require.Equal(t, want[i], got.Provider)
	}
	got, _ := Classify(&accs[0])
	require.Equal(t, "https://api.deepseek.com/user/balance", deepSeekBalanceURL(got.BaseURL))
}
