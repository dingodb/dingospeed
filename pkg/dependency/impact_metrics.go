package dependency

import (
	"github.com/prometheus/client_golang/prometheus"
	"time"
)

// ProbeEvidence must only read memory. A nil source means probes are disabled.
type ProbeEvidence func(ID, time.Time) (Snapshot, bool)

type impactCollector struct {
	business        *Monitor
	probe           ProbeEvidence
	impact, enabled *prometheus.Desc
}

func ImpactCollector(business *Monitor, probe ProbeEvidence) prometheus.Collector {
	return &impactCollector{business: business, probe: probe,
		impact:  prometheus.NewDesc("dingospeed_storage_impact", "Storage evidence assessment, one-hot. Not a whole-mount diagnosis or request policy.", []string{"operation", "assessment"}, nil),
		enabled: prometheus.NewDesc("dingospeed_storage_impact_probe_enabled", "Whether active storage probe evidence is configured for this assessment.", nil, nil),
	}
}
func (c *impactCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.impact; ch <- c.enabled }
func (c *impactCollector) Collect(ch chan<- prometheus.Metric) {
	now := time.Now()
	on := 0.
	if c.probe != nil {
		on = 1
	}
	ch <- prometheus.MustNewConstMetric(c.enabled, prometheus.GaugeValue, on)
	for id, operation := range []string{"read", "write"} {
		var probe Snapshot
		blocked := false
		if c.probe != nil {
			probe, blocked = c.probe(ID(id), now)
		}
		r := AssessStorage(c.business.Snapshot(ID(id), now), probe, c.probe != nil, blocked, now)
		for _, assessment := range assessments {
			value := 0.
			if assessment == r.Assessment {
				value = 1
			}
			ch <- prometheus.MustNewConstMetric(c.impact, prometheus.GaugeValue, value, operation, string(assessment))
		}
	}
}
