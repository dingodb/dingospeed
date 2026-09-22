package dependency

import (
	"github.com/prometheus/client_golang/prometheus"
	"time"
)

type collector struct {
	monitor                 *Monitor
	state, unresolved, last *prometheus.Desc
}

func newCollector(m *Monitor) *collector {
	labels := []string{"dependency", "impact"}
	return &collector{m,
		prometheus.NewDesc("dingospeed_dependency_state", "Observed capability state: 0 unknown, 1 observed healthy, 2 suspect, 3 degraded, 4 recovering. Not a node readiness decision.", labels, nil),
		prometheus.NewDesc("dingospeed_dependency_unresolved", "Observed failure without sufficient positive recovery evidence.", labels, nil),
		prometheus.NewDesc("dingospeed_dependency_last_observation_seconds", "Unix timestamp of last completed instrumented operation; zero means unobserved.", labels, nil),
	}
}
func init() { prometheus.MustRegister(newCollector(Default)) }
func (c *collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.state
	ch <- c.unresolved
	ch <- c.last
}
func (c *collector) Collect(ch chan<- prometheus.Metric) {
	now := time.Now()
	for id, labels := range [][2]string{{"metadata_storage_read", "local_metadata_lookup"}, {"metadata_storage_write", "metadata_cache_persistence"}, {"scheduler_rpc", "cluster_coordination"}} {
		s := c.monitor.Snapshot(ID(id), now)
		unresolved := 0.
		timestamp := 0.
		if s.Unresolved {
			unresolved = 1
		}
		if !s.LastObservation.IsZero() {
			timestamp = float64(s.LastObservation.Unix())
		}
		ch <- prometheus.MustNewConstMetric(c.state, prometheus.GaugeValue, float64(s.State), labels[0], labels[1])
		ch <- prometheus.MustNewConstMetric(c.unresolved, prometheus.GaugeValue, unresolved, labels[0], labels[1])
		ch <- prometheus.MustNewConstMetric(c.last, prometheus.GaugeValue, timestamp, labels[0], labels[1])
	}
}
