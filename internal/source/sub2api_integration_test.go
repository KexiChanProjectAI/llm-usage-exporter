package source

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

// TestLoadAccountsAgainstSchema runs the real query against a database that
// has sub2api's migrations applied. Set LLM_EXPORTER_TEST_DSN to run it; all
// writes happen in a transaction that is rolled back.
func TestLoadAccountsAgainstSchema(t *testing.T) {
	dsn := os.Getenv("LLM_EXPORTER_TEST_DSN")
	if dsn == "" {
		t.Skip("LLM_EXPORTER_TEST_DSN not set")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	defer conn.Close(ctx)
	tx, err := conn.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()

	var proxyID, deletedProxyID int64
	require.NoError(t, tx.QueryRow(ctx, `INSERT INTO proxies (name, protocol, host, port, username, password)
		VALUES ('p', 'socks5h', '10.0.0.1', 1080, 'u', 'p@ss') RETURNING id`).Scan(&proxyID))
	require.NoError(t, tx.QueryRow(ctx, `INSERT INTO proxies (name, protocol, host, port, deleted_at)
		VALUES ('gone', 'http', '10.0.0.2', 3128, NOW()) RETURNING id`).Scan(&deletedProxyID))

	insert := func(name, platform, typ, creds string, proxy *int64, deleted bool) {
		_, err := tx.Exec(ctx, `INSERT INTO accounts (name, platform, type, credentials, proxy_id, deleted_at)
			VALUES ($1, $2, $3, $4::jsonb, $5, CASE WHEN $6 THEN NOW() END)`, name, platform, typ, creds, proxy, deleted)
		require.NoError(t, err)
	}
	insert("it-kimi", "kimi", "apikey", `{"api_key":"k1","account_mode":"coding"}`, &proxyID, false)
	insert("it-zhipu", "zhipu", "apikey", `{"api_key":"k2"}`, &deletedProxyID, false)
	insert("it-deleted", "kimi", "apikey", `{"api_key":"k3"}`, nil, true)
	insert("it-oauth", "anthropic", "oauth", `{"access_token":"t"}`, nil, false)
	insert("it-gemini", "gemini", "apikey", `{"api_key":"k4"}`, nil, false)

	accs, err := LoadAccounts(ctx, tx)
	require.NoError(t, err)
	byName := map[string]Account{}
	for _, a := range accs {
		byName[a.Name] = a
	}
	require.Contains(t, byName, "it-kimi")
	require.Contains(t, byName, "it-zhipu")
	require.NotContains(t, byName, "it-deleted")
	require.NotContains(t, byName, "it-oauth")
	require.NotContains(t, byName, "it-gemini")

	kimi := byName["it-kimi"]
	require.Equal(t, "socks5h://u:p%40ss@10.0.0.1:1080", kimi.ProxyURL)
	require.Equal(t, "k1", kimi.Credential("api_key"))
	require.Equal(t, "active", kimi.Status)
	require.True(t, kimi.Schedulable)
	require.Empty(t, byName["it-zhipu"].ProxyURL, "a soft-deleted proxy is ignored, as in sub2api")
}
