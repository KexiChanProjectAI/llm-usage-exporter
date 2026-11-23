package usage

import (
	"github.com/prometheus/client_golang/prometheus"
)

var (
	accountLabels = []string{"provider", "account_id", "account_name", "window"}
	bucketLabels  = []string{"provider", "granularity", "bucket"}

	descRequests = prometheus.NewDesc("llm_usage_requests",
		"Requests in the trailing window, from sub2api usage_logs.", accountLabels, nil)
	descTokens = prometheus.NewDesc("llm_usage_tokens",
		"Tokens in the trailing window, by type.", append(accountLabels[:4:4], "type"), nil)
	descCost = prometheus.NewDesc("llm_usage_cost_usd",
		"Cost in USD in the trailing window. kind=total: list-price value of the tokens; actual: billed to sub2api users; account: account-side cost.",
		append(accountLabels[:4:4], "kind"), nil)
	descBucketRequests = prometheus.NewDesc("llm_usage_bucket_requests",
		"Requests per provider in one hour or day (bucket = its start in usage.timezone).", bucketLabels, nil)
	descBucketTokens = prometheus.NewDesc("llm_usage_bucket_tokens",
		"Tokens per provider in one hour or day, by type.", append(bucketLabels[:3:3], "type"), nil)
	descBucketCost = prometheus.NewDesc("llm_usage_bucket_cost_usd",
		"Cost in USD per provider in one hour or day, by kind.", append(bucketLabels[:3:3], "kind"), nil)
	descUp = prometheus.NewDesc("llm_usage_up",
		"1 if the last read of sub2api usage_logs succeeded.", nil, nil)
	descLastSuccess = prometheus.NewDesc("llm_usage_last_success_timestamp_seconds",
		"Unix time of the last successful read of usage_logs.", nil, nil)
	descDuration = prometheus.NewDesc("llm_usage_query_duration_seconds",
		"Duration of the last read of usage_logs.", nil, nil)
)

func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{descRequests, descTokens, descCost, descBucketRequests, descBucketTokens,
		descBucketCost, descUp, descLastSuccess, descDuration} {
		ch <- d
	}
}

func gauge(ch chan<- prometheus.Metric, d *prometheus.Desc, v float64, labels ...string) {
	ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, labels...)
}

func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	c.mu.Lock()
	attempted, up, last, dur, synced := c.attempted, c.up, c.lastSuccess, c.lastDur, !c.lastFull.IsZero()
	c.mu.Unlock()
	if attempted {
		v := 0.0
		if up {
			v = 1
		}
		gauge(ch, descUp, v)
		gauge(ch, descDuration, dur.Seconds())
	}
	if !last.IsZero() {
		gauge(ch, descLastSuccess, float64(last.UnixNano())/1e9)
	}
	if !synced {
		return // nothing read yet: no zeros that look like real data
	}
	snap := c.snapshot()
	for _, w := range snap.Windows {
		l := []string{w.Provider, w.AccountID, w.AccountName, w.Window}
		gauge(ch, descRequests, w.Requests, l...)
		for _, t := range w.Tokens() {
			gauge(ch, descTokens, t.Value, append(l, t.Type)...)
		}
		for _, k := range w.Costs() {
			gauge(ch, descCost, k.Value, append(l, k.Kind)...)
		}
	}
	for _, b := range snap.Buckets {
		l := []string{b.Provider, b.Granularity, b.Label}
		gauge(ch, descBucketRequests, b.Requests, l...)
		for _, t := range b.Tokens() {
			gauge(ch, descBucketTokens, t.Value, append(l, t.Type)...)
		}
		for _, k := range b.Costs() {
			gauge(ch, descBucketCost, k.Value, append(l, k.Kind)...)
		}
	}
}
