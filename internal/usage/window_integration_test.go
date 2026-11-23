package usage

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

// TestQuerySpansAgainstSchema checks QuerySpans against plain per-span sums on
// a database with sub2api's schema and some recent usage_logs rows. Set
// LLM_EXPORTER_TEST_DSN to run it; it only reads.
func TestQuerySpansAgainstSchema(t *testing.T) {
	dsn := os.Getenv("LLM_EXPORTER_TEST_DSN")
	if dsn == "" {
		t.Skip("LLM_EXPORTER_TEST_DSN not set")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	defer conn.Close(ctx)

	now := time.Now()
	var ids []int64
	require.NoError(t, conn.QueryRow(ctx, `SELECT COALESCE(array_agg(account_id), '{}') FROM (
		SELECT account_id FROM usage_logs WHERE created_at >= $1 GROUP BY 1 ORDER BY count(*) DESC LIMIT 2) a`,
		now.Add(-7*24*time.Hour)).Scan(&ids))
	if len(ids) == 0 {
		t.Skip("no usage_logs rows in the last 7 days")
	}
	spans := []Span{
		{Since: now.Add(-5 * time.Hour), Until: now},
		{Since: now.Add(-7 * 24 * time.Hour), Until: now.Add(-time.Hour)},
		{Since: now.Add(time.Hour), Until: now.Add(2 * time.Hour)}, // empty
	}
	got, err := QuerySpans(ctx, conn, ids, spans)
	require.NoError(t, err)
	require.Len(t, got, len(spans))
	for i, s := range spans {
		var want Consumed
		require.NoError(t, conn.QueryRow(ctx, `SELECT count(*)::float8,
			COALESCE(sum(COALESCE(input_tokens,0)+COALESCE(output_tokens,0)+COALESCE(cache_creation_tokens,0)+COALESCE(cache_read_tokens,0)),0)::float8,
			COALESCE(sum(total_cost),0)::float8
			FROM usage_logs WHERE account_id = ANY($1) AND created_at >= $2 AND created_at < $3`,
			ids, s.Since, s.Until).Scan(&want.Requests, &want.Tokens, &want.Value))
		require.Equal(t, want.Requests, got[i].Requests, "span %d", i)
		require.Equal(t, want.Tokens, got[i].Tokens, "span %d", i)
		require.InDelta(t, want.Value, got[i].Value, 1e-9, "span %d", i)
	}
	require.Zero(t, got[2].Requests)
	require.Greater(t, got[1].Requests, 0.0)
}
