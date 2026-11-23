// Package source reads quota-capable accounts from the sub2api database.
package source

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/KexiChanProjectAI/llm-usage-exporter/internal/config"

	"github.com/jackc/pgx/v5"
)

// Querier is satisfied by *pgxpool.Pool, *pgx.Conn and pgx.Tx.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Account is the subset of a sub2api accounts row the exporter needs.
type Account struct {
	ID int64
	// LabelID overrides the account_id label; used for config-file accounts.
	LabelID     string
	Name        string
	Platform    string
	Type        string
	Status      string
	Schedulable bool
	Credentials map[string]any
	// ProxyURL is the account's sub2api proxy (accounts.proxy_id), "" when the
	// account has none or the proxy row is gone.
	ProxyURL string
}

// IDLabel is the account_id label value.
func (a *Account) IDLabel() string {
	if a.LabelID != "" {
		return a.LabelID
	}
	return strconv.FormatInt(a.ID, 10)
}

// Credential returns credentials[key] when it is a string.
func (a *Account) Credential(key string) string {
	if a == nil || a.Credentials == nil {
		return ""
	}
	s, _ := a.Credentials[key].(string)
	return s
}

// Platforms whose accounts can carry a supported quota. openai, anthropic and
// deepseek are included because sub2api lets an OpenCode Go key be mounted on
// them through base_url.
var Platforms = []string{
	"zhipu", "minimax", "kimi", "opencode_go", "command_code", "cline",
	"openai", "anthropic", "deepseek",
}

// Only API key style accounts carry a data-plane key that the quota endpoints
// accept; OAuth rows are never loaded so their tokens never leave the DB.
var accountTypes = []string{"apikey", "upstream"}

const accountsQuery = `
SELECT a.id, a.name, a.platform, a.type, a.status, a.schedulable, a.credentials,
       p.protocol, p.host, p.port, p.username, p.password
FROM accounts a
LEFT JOIN proxies p ON p.id = a.proxy_id AND p.deleted_at IS NULL
WHERE a.deleted_at IS NULL
  AND a.platform = ANY($1)
  AND a.type = ANY($2)
ORDER BY a.id`

// LoadAccounts returns every non-deleted API key account on the candidate
// platforms. Classification into providers happens in package provider.
func LoadAccounts(ctx context.Context, db Querier) ([]Account, error) {
	rows, err := db.Query(ctx, accountsQuery, Platforms, accountTypes)
	if err != nil {
		return nil, fmt.Errorf("query accounts: %w", err)
	}
	defer rows.Close()

	var out []Account
	for rows.Next() {
		var (
			acc      Account
			credsRaw []byte
			protocol *string
			host     *string
			port     *int64
			username *string
			password *string
		)
		if err := rows.Scan(&acc.ID, &acc.Name, &acc.Platform, &acc.Type, &acc.Status, &acc.Schedulable,
			&credsRaw, &protocol, &host, &port, &username, &password); err != nil {
			return nil, fmt.Errorf("scan account: %w", err)
		}
		if len(credsRaw) > 0 {
			if err := json.Unmarshal(credsRaw, &acc.Credentials); err != nil {
				// One malformed row must not hide every other account.
				acc.Credentials = nil
			}
		}
		if protocol != nil && host != nil && port != nil {
			acc.ProxyURL = proxyURL(*protocol, *host, int(*port), deref(username), deref(password))
		}
		out = append(out, acc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read accounts: %w", err)
	}
	return out, nil
}

// proxyURL mirrors sub2api's Proxy.URL(): credentials only when both parts are set.
func proxyURL(protocol, host string, port int, username, password string) string {
	u := &url.URL{Scheme: protocol, Host: net.JoinHostPort(host, strconv.Itoa(port))}
	if username != "" && password != "" {
		u.User = url.UserPassword(username, password)
	}
	return u.String()
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// StaticAccounts turns config-file accounts into Accounts. Their account_id
// label is "cfg:<name>" and their status "config".
func StaticAccounts(list []config.StaticAccount) []Account {
	out := make([]Account, 0, len(list))
	for _, a := range list {
		creds := map[string]any{"api_key": strings.TrimSpace(a.APIKey)}
		if a.BaseURL != "" {
			creds["base_url"] = strings.TrimSpace(a.BaseURL)
		}
		if a.AccountMode != "" {
			creds["account_mode"] = a.AccountMode
		}
		proxy := strings.TrimSpace(a.Proxy)
		if proxy == config.ProxyDirect {
			proxy = ""
		}
		out = append(out, Account{
			LabelID: "cfg:" + a.Name, Name: a.Name, Platform: a.Provider, Type: "apikey",
			Status: "config", Schedulable: true, Credentials: creds, ProxyURL: proxy,
		})
	}
	return out
}
