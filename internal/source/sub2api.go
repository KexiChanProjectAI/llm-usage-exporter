// Package source reads quota-capable accounts from the sub2api database.
package source

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strconv"

	"github.com/jackc/pgx/v5"
)

// Querier is satisfied by *pgxpool.Pool, *pgx.Conn and pgx.Tx.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Account is the subset of a sub2api accounts row the exporter needs.
type Account struct {
	ID          int64
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
