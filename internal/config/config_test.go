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
	_, err := Parse([]byte("database: {dsn: x}\nproxy: direct\nproviders: {kimi: {proxy: 'socks5h://u:p@h:1', interval: 1m}}\n"))
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
