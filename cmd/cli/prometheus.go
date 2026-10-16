package cli

import "github.com/prometheus/client_golang/prometheus"

const (
	metricsLabelListener       = "listener"
	metricsLabelClientSourceIP = "client_source_ip"
	metricsLabelClientMac      = "client_mac"
	metricsLabelClientHostname = "client_hostname"
	metricsLabelUpstream       = "upstream"
	metricsLabelRRType         = "rr_type"
	metricsLabelRCode          = "rcode"
)

// statsVersion represent ctrld version.
var statsVersion = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "ctrld_build_info",
	Help: "Version of ctrld process.",
}, []string{"gitref", "goversion", "version"})

// statsTimeStart represents start time of ctrld service.
var statsTimeStart = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "ctrld_time_seconds",
	Help: "Start time of the ctrld process since unix epoch in seconds.",
})

var statsQueriesCountLabels = []string{
	metricsLabelListener,
	metricsLabelClientSourceIP,
	metricsLabelClientMac,
	metricsLabelClientHostname,
	metricsLabelUpstream,
	metricsLabelRRType,
	metricsLabelRCode,
}

// statsQueriesCount counts total number of queries.
var statsQueriesCount = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "ctrld_queries_count",
	Help: "Total number of queries.",
}, statsQueriesCountLabels)

// statsClientQueriesCount counts total number of queries of a client.
//
// The labels "client_source_ip", "client_mac", "client_hostname" are unbounded,
// thus this stat is highly inefficient if there are many devices.
var statsClientQueriesCount = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "ctrld_client_queries_count",
	Help: "Total number queries of a client.",
}, []string{metricsLabelClientSourceIP, metricsLabelClientMac, metricsLabelClientHostname})

// statsDomainPrefetchPredictions counts the number of speculative prefetches
// the domain-graph predictor has scheduled. Incremented per neighbor
// prefetched (so a single query that triggers 5 neighbor prefetches
// increments by 5). Exposed unconditionally so it can be monitored
// independently of MetricsQueryStats.
var statsDomainPrefetchPredictions = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "ctrld_domain_prefetch_predictions_total",
	Help: "Total number of speculative prefetches scheduled by the domain-graph predictor.",
})

// statsLocalHistoryReplays counts the total number of queries replayed from
// the LOCAL query log file into the domain graph on startup. Each replay =
// one historical query pre-seeding the co-occurrence graph. Exposed
// unconditionally so you can verify the local-log replay actually happened.
var statsLocalHistoryReplays = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "ctrld_local_history_replays_total",
	Help: "Total number of queries replayed from the local query log into the domain graph on startup.",
})

// WithLabelValuesInc increases prometheus counter by 1 if query stats is enabled.
func (p *prog) WithLabelValuesInc(c *prometheus.CounterVec, lvs ...string) {
	if p.metricsQueryStats.Load() {
		c.WithLabelValues(lvs...).Inc()
	}
}
