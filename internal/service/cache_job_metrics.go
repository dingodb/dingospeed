package service

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"time"
)

var cacheStopDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Name: "dingospeed_cache_stop_seconds", Help: "Time from accepted stop intent to joined download completion", Buckets: []float64{.01, .05, .1, .5, 1, 2, 5, 10, 30},
}, []string{"intent"})
var cacheTransitions = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "dingospeed_cache_transitions_total", Help: "Durably committed cache lifecycle transitions",
}, []string{"state"})
var cachePersistenceFailures = promauto.NewCounter(prometheus.CounterOpts{
	Name: "dingospeed_cache_snapshot_write_failures_total", Help: "Failed durable cache snapshot writes",
})

func init() {
	for _, state := range []string{"pausing", "resuming", "canceling"} {
		promauto.NewGaugeFunc(prometheus.GaugeOpts{Name: "dingospeed_cache_transition_oldest_seconds", Help: "Oldest active cache transition", ConstLabels: prometheus.Labels{"state": state}}, func() float64 {
			cacheStateMu.Lock()
			defer cacheStateMu.Unlock()
			oldest := 0.0
			for _, execution := range cacheExecutions {
				if execution.state != state {
					continue
				}
				since := execution.started
				if !execution.stopStarted.IsZero() {
					since = execution.stopStarted
				}
				age := time.Since(since).Seconds()
				if age > oldest {
					oldest = age
				}
			}
			return oldest
		})
	}
}
