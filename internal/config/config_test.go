package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDefaults(t *testing.T) {
	cfg, err := Parse([]byte("database: {dsn: postgres://x}\n"))
	require.NoError(t, err)
	require.Equal(t, ":9877", cfg.Listen)
	require.Equal(t, 6*time.Hour, cfg.AuthBlacklist.Duration)
	require.Equal(t, []int{401}, cfg.AuthBlacklist.StatusCodes)
	require.True(t, cfg.UseAccountProxyEnabled())
	require.Equal(t, Providers, cfg.EnabledProviders())
	require.Equal(t, 5*time.Minute, cfg.Provider(ProviderCline).Interval)
}

func TestValidation(t *testing.T) {
	bad := map[string]string{
		"no dsn":            "listen: ':1'\n",
		"unknown provider":  "database: {dsn: x}\nproviders: {glm: {}}\n",
		"unknown field":     "database: {dsn: x}\nproxyy: http://a\n",
		"bad proxy scheme":  "database: {dsn: x}\nproxy: ftp://a:1\n",
		"bad provider prox": "database: {dsn: x}\nproviders: {kimi: {proxy: 'socks4://a:1'}}\n",
		"interval too low":  "database: {dsn: x}\nproviders: {kimi: {interval: 10s}}\n",
		"opencode floor":    "database: {dsn: x}\nproviders: {opencode_go: {interval: 2m}}\n",
	}
	for name, doc := range bad {
		_, err := Parse([]byte(doc))
		require.Error(t, err, name)
	}
	_, err := Parse([]byte("database: {dsn: x}\ninstance: ' x'\n"))
	require.Error(t, err)
	cfg, err := Parse([]byte("database: {dsn: x}\ninstance: first\n"))
	require.NoError(t, err)
	require.Equal(t, "first", cfg.Instance)
	_, err = Parse([]byte("database: {dsn: x}\nproxy: direct\nproviders: {kimi: {proxy: 'socks5h://u:p@h:1', interval: 1m}}\n"))
	require.NoError(t, err)
}

func TestExpandEnv(t *testing.T) {
	t.Setenv("PGPW", "s3cr$t")
	out, err := expandEnv("dsn: postgres://u:${PGPW}@h/db $NOT_EXPANDED")
	require.NoError(t, err)
	require.Equal(t, "dsn: postgres://u:s3cr$t@h/db $NOT_EXPANDED", out)
	_, err = expandEnv("${SURELY_MISSING_VAR_123}")
	require.Error(t, err)
}

func TestStaticAccounts(t *testing.T) {
	cfg, err := Parse([]byte("database: {dsn: x}\naccounts:\n  - {name: ds, provider: deepseek, api_key: sk-1}\n  - {name: g, provider: zhipu, api_key: k2, proxy: direct}\n"))
	require.NoError(t, err)
	require.Equal(t, "", cfg.Accounts[0].AccountMode)
	require.Equal(t, "coding", cfg.Accounts[1].AccountMode)
	require.Equal(t, 6*time.Hour, cfg.Collect.UnsupportedRetry)

	bad := map[string]string{
		"no name":   "accounts: [{provider: deepseek, api_key: k}]",
		"dup name":  "accounts: [{name: a, provider: deepseek, api_key: k}, {name: a, provider: kimi, api_key: k}]",
		"provider":  "accounts: [{name: a, provider: openai, api_key: k}]",
		"no key":    "accounts: [{name: a, provider: deepseek}]",
		"bad proxy": "accounts: [{name: a, provider: deepseek, api_key: k, proxy: 'ftp://x:1'}]",
	}
	for name, doc := range bad {
		_, err := Parse([]byte("database: {dsn: x}\n" + doc + "\n"))
		require.Error(t, err, name)
	}
}

func TestUsage(t *testing.T) {
	cfg, err := Parse([]byte("database: {dsn: x}\n"))
	require.NoError(t, err)
	u := cfg.Usage
	require.True(t, u.IsEnabled())
	require.Equal(t, []int{1, 7, 30}, u.Windows)
	require.Equal(t, 24, u.HourlyBuckets)
	require.Equal(t, 7, u.DailyBuckets)
	require.Equal(t, "UTC", u.Timezone)
	require.Equal(t, 5*time.Minute, u.Interval)

	bad := map[string]string{
		"tz":       "usage: {timezone: Mars/Base}",
		"local tz": "usage: {timezone: Local}",
		"window 0": "usage: {windows: [0]}",
		"dup":      "usage: {windows: [7, 7]}",
		"interval": "usage: {interval: 10s}",
		"resync":   "usage: {interval: 10m, resync_interval: 5m}",
		"buckets":  "usage: {hourly_buckets: -1}",
	}
	for name, doc := range bad {
		_, err := Parse([]byte("database: {dsn: x}\n" + doc + "\n"))
		require.Error(t, err, name)
	}
}
