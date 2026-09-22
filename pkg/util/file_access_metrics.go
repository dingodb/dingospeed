package util

import "github.com/prometheus/client_golang/prometheus"

// Like the existing application metrics, register once at package initialization.
// The existing server.metrics flag controls exposure of /metrics. Router creation
// never registers this collector, so multiple routers cannot register it twice.
func init() {
	prometheus.MustRegister(newFileAccessCollector())
}

type fileAccessCollector struct {
	desc *prometheus.Desc
}

func newFileAccessCollector() *fileAccessCollector {
	return &fileAccessCollector{desc: prometheus.NewDesc(
		"dingospeed_storage_access_errors_total",
		"Observed filesystem access errors in instrumented metadata operations since process start; not a storage health check.",
		[]string{"operation", "kind"}, nil,
	)}
}

func (c *fileAccessCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c *fileAccessCollector) Collect(ch chan<- prometheus.Metric) {
	// Snapshot existing atomics only: scraping performs no filesystem access,
	// logging, retries, or changes to request handling.
	for _, observation := range FileAccessObservations() {
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.CounterValue,
			float64(observation.Count), observation.Operation, string(observation.Kind))
	}
}
