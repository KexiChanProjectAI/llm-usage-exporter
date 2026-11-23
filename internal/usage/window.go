package usage

import (
	"context"
	"fmt"
	"time"
)

// Span is a half-open time range [Since, Until).
type Span struct {
	Since, Until time.Time
}

// Consumed is what sub2api logged for a set of accounts in one span.
type Consumed struct {
	Requests float64
	// Tokens counts every type: input, output, cache writes and cache reads.
	Tokens float64
	// Value is the list-price value of the tokens in USD (usage_logs.total_cost).
	Value float64
}

// One row per span, in input order, zeros when nothing was logged. The
// (account_id, created_at) index serves each span.
const spanQuery = `
SELECT w.i,
       count(u.account_id)::float8,
       COALESCE(sum(COALESCE(u.input_tokens, 0) + COALESCE(u.output_tokens, 0)
                   + COALESCE(u.cache_creation_tokens, 0) + COALESCE(u.cache_read_tokens, 0)), 0)::float8,
       COALESCE(sum(u.total_cost), 0)::float8
FROM unnest($2::timestamptz[], $3::timestamptz[]) WITH ORDINALITY AS w(since, until, i)
LEFT JOIN usage_logs u
       ON u.account_id = ANY($1) AND u.created_at >= w.since AND u.created_at < w.until
GROUP BY w.i
ORDER BY w.i`

// QuerySpans sums the usage of the given accounts over each span.
func QuerySpans(ctx context.Context, db Querier, accountIDs []int64, spans []Span) ([]Consumed, error) {
	if len(spans) == 0 {
		return nil, nil
	}
	since := make([]time.Time, len(spans))
	until := make([]time.Time, len(spans))
	for i, s := range spans {
		since[i], until[i] = s.Since, s.Until
	}
	rows, err := db.Query(ctx, spanQuery, accountIDs, since, until)
	if err != nil {
		return nil, fmt.Errorf("query usage_logs: %w", err)
	}
	defer rows.Close()
	out := make([]Consumed, len(spans))
	for rows.Next() {
		var (
			i int64
			c Consumed
		)
		if err := rows.Scan(&i, &c.Requests, &c.Tokens, &c.Value); err != nil {
			return nil, fmt.Errorf("scan usage: %w", err)
		}
		if i < 1 || int(i) > len(spans) {
			return nil, fmt.Errorf("scan usage: span %d out of range", i)
		}
		out[i-1] = c
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read usage: %w", err)
	}
	return out, nil
}
