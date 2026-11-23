package source

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/KexiChanProjectAI/llm-usage-exporter/internal/config"
)

func TestStaticAccounts(t *testing.T) {
	accs := StaticAccounts([]config.StaticAccount{
		{Name: "DeepSeek Cyber", Provider: "deepseek", APIKey: " sk-1 "},
		{Name: "g", Provider: "zhipu", APIKey: "k", AccountMode: "coding", BaseURL: "https://api.z.ai/api/anthropic", Proxy: "socks5h://p:1"},
		{Name: "d", Provider: "deepseek", APIKey: "k", Proxy: "direct"},
	})
	require.Len(t, accs, 3)
	require.Equal(t, "cfg:DeepSeek Cyber", accs[0].IDLabel())
	require.Equal(t, "sk-1", accs[0].Credential("api_key"))
	require.Equal(t, "apikey", accs[0].Type)
	require.Equal(t, "config", accs[0].Status)
	require.Equal(t, "https://api.z.ai/api/anthropic", accs[1].Credential("base_url"))
	require.Equal(t, "coding", accs[1].Credential("account_mode"))
	require.Equal(t, "socks5h://p:1", accs[1].ProxyURL)
	require.Empty(t, accs[2].ProxyURL)

	sub := Account{ID: 42}
	require.Equal(t, "42", sub.IDLabel())
}
