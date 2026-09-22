package dependency

import (
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func TestStorageImpactMatrix(t *testing.T) {
	now := time.Unix(1000, 0)
	healthy := Snapshot{State: ObservedHealthy, LastObservation: now}
	failure := Snapshot{State: Suspect, LastObservation: now, LastFailure: now, Unresolved: true}
	recovering := failure
	recovering.State = Recovering
	stale := failure
	stale.LastObservation = now.Add(-3 * time.Minute)
	stale.LastFailure = stale.LastObservation
	earlierHealthy := healthy
	earlierHealthy.LastObservation = now.Add(-time.Second)
	earlierFailure := failure
	earlierFailure.LastObservation = now.Add(-40 * time.Second)
	earlierFailure.LastFailure = earlierFailure.LastObservation
	cases := []struct {
		name             string
		business, probe  Snapshot
		enabled, blocked bool
		want             Assessment
	}{
		{"nothing observed", Snapshot{}, Snapshot{}, false, false, ImpactUnknown},
		{"both healthy", healthy, healthy, true, false, ImpactHealthy},
		{"disabled is not healthy", healthy, healthy, false, false, ImpactPartial},
		{"no business traffic", Snapshot{}, healthy, true, false, ImpactPartial},
		{"business failed probe disabled", failure, failure, false, true, ImpactBusinessFailure},
		{"business failed probe unknown", failure, Snapshot{}, true, false, ImpactBusinessFailure},
		{"successful probe after failure", failure, healthy, true, false, ImpactBusinessFailureProbeHealthy},
		{"successful probe before failure", failure, earlierHealthy, true, false, ImpactBusinessFailure},
		{"probe failure only", healthy, failure, true, false, ImpactProbeFailure},
		{"temporally aligned failures", failure, failure, true, false, ImpactCorrelatedFailure},
		{"separate failure times", failure, earlierFailure, true, false, ImpactUncorrelatedFailures},
		{"stale is unresolved", stale, healthy, true, false, ImpactUnresolved},
		{"recovering is not recovered", recovering, healthy, true, false, ImpactUnresolved},
		{"expired but still blocked", healthy, stale, true, true, ImpactProbeFailure},
		{"new business error during old blockage", failure, stale, true, true, ImpactCorrelatedFailure},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := AssessStorage(tc.business, tc.probe, tc.enabled, tc.blocked, now)
			if got.Assessment != tc.want {
				t.Fatalf("got %+v want %s", got, tc.want)
			}
		})
	}
}

func TestStorageImpactRecoveryAndExpiry(t *testing.T) {
	business, probe := &Monitor{}, &Monitor{}
	base := time.Unix(1000, 0)
	for _, sec := range []int{0, 5, 10} {
		at := base.Add(time.Duration(sec) * time.Second)
		business.Observe(MetadataRead, false, at)
		probe.Observe(MetadataRead, false, at)
	}
	assess := func(sec int) Assessment {
		now := base.Add(time.Duration(sec) * time.Second)
		return AssessStorage(business.Snapshot(MetadataRead, now), probe.Snapshot(MetadataRead, now), true, false, now).Assessment
	}
	if assess(10) != ImpactCorrelatedFailure {
		t.Fatal("paired failures lost")
	}
	before := business.Snapshot(MetadataRead, base.Add(10*time.Second))
	for _, sec := range []int{15, 20, 25} {
		probe.Observe(MetadataRead, true, base.Add(time.Duration(sec)*time.Second))
	}
	if assess(25) != ImpactBusinessFailureProbeHealthy {
		t.Fatal("probe recovery cleared business failure")
	}
	if business.Snapshot(MetadataRead, base.Add(25*time.Second)) != before {
		t.Fatal("assessment modified source")
	}
	if assess(180) != ImpactUnresolved {
		t.Fatal("no traffic treated as recovery")
	}
	if s := business.Snapshot(MetadataWrite, base); s.State != Unknown {
		t.Fatal("read affected write")
	}
	if s := business.Snapshot(Scheduler, base); s.State != Unknown {
		t.Fatal("storage affected scheduler")
	}
}

func TestImpactCollectorBoundedReadOnlyAndConcurrent(t *testing.T) {
	business, probe := &Monitor{}, &Monitor{}
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(ImpactCollector(business, func(id ID, now time.Time) (Snapshot, bool) { return probe.Snapshot(id, now), false }))
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				business.Observe(MetadataRead, j%2 == 0, time.Now())
				probe.Observe(MetadataWrite, j%2 == 0, time.Now())
				if _, err := registry.Gather(); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	now := time.Now()
	before := business.Snapshot(MetadataRead, now)
	metrics, err := registry.Gather()
	if err != nil || len(metrics) != 2 {
		t.Fatalf("metrics: %v %d", err, len(metrics))
	}
	for _, family := range metrics {
		if family.GetName() != "dingospeed_storage_impact" {
			continue
		}
		if len(family.Metric) != 18 {
			t.Fatal("unbounded assessment labels")
		}
		sums := map[string]float64{}
		for _, m := range family.Metric {
			operation := ""
			for _, l := range m.Label {
				if l.GetName() == "operation" {
					operation = l.GetValue()
				}
			}
			sums[operation] += m.Gauge.GetValue()
		}
		if sums["read"] != 1 || sums["write"] != 1 {
			t.Fatalf("not one-hot: %v", sums)
		}
	}
	if business.Snapshot(MetadataRead, now) != before {
		t.Fatal("scrape changed evidence")
	}
}
