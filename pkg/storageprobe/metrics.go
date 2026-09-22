package storageprobe

import (
	"dingospeed/pkg/dependency"
	"github.com/prometheus/client_golang/prometheus"
	"time"
)

type collector struct {
	p                                    *Probe
	state, unresolved, blocked, failures *prometheus.Desc
}

func Collector(p *Probe) prometheus.Collector {
	labels := []string{"operation"}
	return &collector{p,
		prometheus.NewDesc("dingospeed_storage_probe_state", "Dedicated probe evidence; same state values as dependency_state; not business readiness.", labels, nil),
		prometheus.NewDesc("dingospeed_storage_probe_unresolved", "Probe failure lacking positive recovery evidence.", labels, nil),
		prometheus.NewDesc("dingospeed_storage_probe_blocked", "Timed-out probe operation still in flight; no replacement is launched.", labels, nil),
		prometheus.NewDesc("dingospeed_storage_probe_errors_total", "Failed probe attempts, counted once, with bounded classification.", []string{"operation", "kind"}, nil),
	}
}
func (c *collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.state
	ch <- c.unresolved
	ch <- c.blocked
	ch <- c.failures
}
func (c *collector) Collect(ch chan<- prometheus.Metric) {
	for id, op := range []string{"read", "write"} {
		s := c.p.monitor.Snapshot(dependency.ID(id), time.Now())
		status := c.p.snapshot(id)
		u, b := 0., 0.
		if s.Unresolved {
			u = 1
		}
		if status.inFlight && status.timedOut {
			b = 1
		}
		ch <- prometheus.MustNewConstMetric(c.state, prometheus.GaugeValue, float64(s.State), op)
		ch <- prometheus.MustNewConstMetric(c.unresolved, prometheus.GaugeValue, u, op)
		ch <- prometheus.MustNewConstMetric(c.blocked, prometheus.GaugeValue, b, op)
		for i, kind := range kinds {
			ch <- prometheus.MustNewConstMetric(c.failures, prometheus.CounterValue, float64(status.errors[i]), op, kind)
		}
	}
}
