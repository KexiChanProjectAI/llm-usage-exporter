// Package config loads the exporter's YAML configuration.
package config

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Provider IDs. They match sub2api's accounts.platform values.
const (
	ProviderZhipu       = "zhipu" // GLM Coding Plan (bigmodel.cn / z.ai)
	ProviderMiniMax     = "minimax"
	ProviderKimi        = "kimi"
	ProviderOpenCodeGo  = "opencode_go"
	ProviderCommandCode = "command_code"
	ProviderCline       = "cline"
)

// Providers lists every supported provider ID.
var Providers = []string{
	ProviderZhipu, ProviderMiniMax, ProviderKimi,
	ProviderOpenCodeGo, ProviderCommandCode, ProviderCline,
}

// ProxyDirect is a proxy value that forces a direct connection and stops the
// proxy fallback chain.
const ProxyDirect = "direct"

type Config struct {
	Listen      string `yaml:"listen"`
	MetricsPath string `yaml:"metrics_path"`

	Database Database `yaml:"database"`

	// Proxy is the global proxy URL for upstream quota requests. Empty means
	// none; ProxyDirect forces a direct connection.
	Proxy string `yaml:"proxy"`
	// UseAccountProxy makes an account's sub2api proxy (accounts.proxy_id)
	// take precedence over the global proxy.
	UseAccountProxy *bool `yaml:"use_account_proxy"`

	Collect       Collect                   `yaml:"collect"`
	AuthBlacklist AuthBlacklist             `yaml:"auth_blacklist"`
	Providers     map[string]ProviderConfig `yaml:"providers"`
}

type Database struct {
	DSN            string        `yaml:"dsn"`
	ReloadInterval time.Duration `yaml:"reload_interval"`
	QueryTimeout   time.Duration `yaml:"query_timeout"`
}

type Collect struct {
	// Concurrency caps in-flight upstream quota fetches.
	Concurrency int `yaml:"concurrency"`
	// Timeout bounds one upstream HTTP request.
	Timeout time.Duration `yaml:"timeout"`
	// MaxStaleness stops exporting quota values whose last successful fetch is
	// older than this. Zero disables the cut-off.
	MaxStaleness time.Duration `yaml:"max_staleness"`
}

type AuthBlacklist struct {
	// Duration is how long an API key that got one of StatusCodes is skipped.
	// The key is retried earlier as soon as it changes in sub2api.
	Duration    time.Duration `yaml:"duration"`
	StatusCodes []int         `yaml:"status_codes"`
}

type ProviderConfig struct {
	Enabled  *bool         `yaml:"enabled"`
	Interval time.Duration `yaml:"interval"`
	// Proxy overrides every other proxy setting for this provider. Empty means
	// not set; ProxyDirect forces a direct connection.
	Proxy string `yaml:"proxy"`
}

// IsEnabled reports whether the provider is enabled (default true).
func (p ProviderConfig) IsEnabled() bool { return p.Enabled == nil || *p.Enabled }

// UseAccountProxyEnabled reports the effective use_account_proxy (default true).
func (c *Config) UseAccountProxyEnabled() bool { return c.UseAccountProxy == nil || *c.UseAccountProxy }

// Provider returns the provider's config with defaults applied.
func (c *Config) Provider(id string) ProviderConfig {
	return c.Providers[id]
}

// EnabledProviders returns the enabled provider IDs.
func (c *Config) EnabledProviders() []string {
	var out []string
	for _, id := range Providers {
		if c.Provider(id).IsEnabled() {
			out = append(out, id)
		}
	}
	return out
}

const (
	defaultInterval    = 5 * time.Minute
	minInterval        = time.Minute
	minOpenCodeGoInter = 5 * time.Minute // sub2api's floor for the OpenCode usage endpoint
)

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandEnv replaces ${VAR} references. Bare $VAR is left untouched so that
// literal dollar signs in values survive.
func expandEnv(s string) (string, error) {
	var missing []string
	out := envRef.ReplaceAllStringFunc(s, func(m string) string {
		name := envRef.FindStringSubmatch(m)[1]
		v, ok := os.LookupEnv(name)
		if !ok {
			missing = append(missing, name)
		}
		return v
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("environment variables not set: %s", strings.Join(missing, ", "))
	}
	return out, nil
}

// Load reads, expands and validates the config file at path.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	expanded, err := expandEnv(string(raw))
	if err != nil {
		return nil, err
	}
	return Parse([]byte(expanded))
}

// Parse decodes and validates a config document (already env-expanded).
func Parse(data []byte) (*Config, error) {
	cfg := &Config{}
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = ":9877"
	}
	if c.MetricsPath == "" {
		c.MetricsPath = "/metrics"
	}
	if c.Database.ReloadInterval == 0 {
		c.Database.ReloadInterval = 2 * time.Minute
	}
	if c.Database.QueryTimeout == 0 {
		c.Database.QueryTimeout = 15 * time.Second
	}
	if c.Collect.Concurrency == 0 {
		c.Collect.Concurrency = 8
	}
	if c.Collect.Timeout == 0 {
		c.Collect.Timeout = 15 * time.Second
	}
	if c.Collect.MaxStaleness == 0 {
		c.Collect.MaxStaleness = time.Hour
	}
	if c.AuthBlacklist.Duration == 0 {
		c.AuthBlacklist.Duration = 6 * time.Hour
	}
	if c.AuthBlacklist.StatusCodes == nil {
		c.AuthBlacklist.StatusCodes = []int{401}
	}
	if c.Providers == nil {
		c.Providers = map[string]ProviderConfig{}
	}
	for _, id := range Providers {
		p := c.Providers[id]
		if p.Interval == 0 {
			p.Interval = defaultInterval
		}
		c.Providers[id] = p
	}
}

func (c *Config) validate() error {
	if strings.TrimSpace(c.Database.DSN) == "" {
		return fmt.Errorf("database.dsn is required")
	}
	if c.Collect.Concurrency < 1 {
		return fmt.Errorf("collect.concurrency must be >= 1")
	}
	if c.Collect.MaxStaleness < 0 {
		return fmt.Errorf("collect.max_staleness must not be negative")
	}
	if !strings.HasPrefix(c.MetricsPath, "/") {
		return fmt.Errorf("metrics_path must start with /")
	}
	if err := ValidateProxy(c.Proxy); err != nil {
		return fmt.Errorf("proxy: %w", err)
	}
	known := map[string]bool{}
	for _, id := range Providers {
		known[id] = true
	}
	for id, p := range c.Providers {
		if !known[id] {
			return fmt.Errorf("providers.%s: unknown provider (supported: %s)", id, strings.Join(Providers, ", "))
		}
		if p.Interval < minInterval {
			return fmt.Errorf("providers.%s.interval must be >= %s", id, minInterval)
		}
		if id == ProviderOpenCodeGo && p.Interval < minOpenCodeGoInter {
			return fmt.Errorf("providers.%s.interval must be >= %s", id, minOpenCodeGoInter)
		}
		if err := ValidateProxy(p.Proxy); err != nil {
			return fmt.Errorf("providers.%s.proxy: %w", id, err)
		}
	}
	return nil
}

// ValidateProxy accepts "", ProxyDirect, or an http/https/socks5/socks5h URL.
func ValidateProxy(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == ProxyDirect {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid proxy URL")
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "socks5", "socks5h":
	default:
		return fmt.Errorf("unsupported proxy scheme %q (want http, https, socks5 or socks5h)", u.Scheme)
	}
	if u.Hostname() == "" {
		return fmt.Errorf("proxy URL has no host")
	}
	return nil
}
