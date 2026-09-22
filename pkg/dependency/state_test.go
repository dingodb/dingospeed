package dependency

import (
	"github.com/prometheus/client_golang/prometheus"
	"sync"
	"testing"
	"time"
)

func TestScopeAndRecovery(t *testing.T) {
	m := &Monitor{}
	now := time.Unix(1000, 0)
	if m.Snapshot(MetadataRead, now).State != Unknown {
		t.Fatal("unobserved is not healthy")
	}
	m.Observe(MetadataRead, false, now)
	for i := 0; i < 100; i++ {
		m.Observe(MetadataRead, false, now)
	}
	if m.Snapshot(MetadataRead, now).State != Suspect {
		t.Fatal("burst promoted to dependency failure")
	}
	m.Observe(MetadataRead, false, now.Add(5*time.Second))
	m.Observe(MetadataRead, false, now.Add(10*time.Second))
	if m.Snapshot(MetadataRead, now.Add(10*time.Second)).State != Degraded {
		t.Fatal("sustained failure not identified")
	}
	if m.Snapshot(MetadataWrite, now).State != Unknown || m.Snapshot(Scheduler, now).State != Unknown {
		t.Fatal("failure scope spread")
	}
	stale := m.Snapshot(MetadataRead, now.Add(3*time.Minute))
	if stale.State != Unknown || !stale.Unresolved {
		t.Fatal("silence treated as recovery")
	}
	for _, sec := range []int{180, 185} {
		m.Observe(MetadataRead, true, now.Add(time.Duration(sec)*time.Second))
	}
	if s := m.Snapshot(MetadataRead, now.Add(185*time.Second)); s.State != Recovering || !s.Unresolved {
		t.Fatal("premature recovery")
	}
	m.Observe(MetadataRead, true, now.Add(190*time.Second))
	if s := m.Snapshot(MetadataRead, now.Add(190*time.Second)); s.State != ObservedHealthy || s.Unresolved {
		t.Fatal("positive recovery not accepted")
	}
}

func TestEvidenceSlidingWindow(t *testing.T) {
	for _, success := range []bool{false, true} {
		m := &Monitor{}
		base := time.Unix(1000, 0)
		if success {
			m.Observe(MetadataRead, false, base.Add(-time.Second))
		}
		for _, sec := range []int{0, 25, 31, 35} {
			m.Observe(MetadataRead, success, base.Add(time.Duration(sec)*time.Second))
		}
		want := Degraded
		if success {
			want = ObservedHealthy
		}
		if got := m.Snapshot(MetadataRead, base.Add(35*time.Second)); got.State != want {
			t.Fatalf("success=%v: recent samples at 25,31,35 were lost: %+v", success, got)
		}
	}
	var e evidence
	base := time.Unix(1000, 0)
	for i := 0; i < 10000; i++ {
		e.add(base.Add(time.Duration(i) * 100 * time.Millisecond))
		if e.samples > 31 || !e.times[e.samples-1].After(base.Add(-time.Second)) {
			t.Fatal("window is not bounded")
		}
	}
}

func TestWindowsAndInterruptedRecovery(t *testing.T) {
	m := &Monitor{}
	now := time.Unix(1000, 0)
	for _, sec := range []int{0, 40, 80} {
		m.Observe(Scheduler, false, now.Add(time.Duration(sec)*time.Second))
	}
	if m.Snapshot(Scheduler, now.Add(80*time.Second)).State != Suspect {
		t.Fatal("sparse errors aggregated outside window")
	}
	m.Observe(Scheduler, true, now.Add(81*time.Second))
	m.Observe(Scheduler, true, now.Add(86*time.Second))
	m.Observe(Scheduler, false, now.Add(90*time.Second))
	m.Observe(Scheduler, true, now.Add(91*time.Second))
	if s := m.Snapshot(Scheduler, now.Add(91*time.Second)); !s.Unresolved || s.State != Recovering {
		t.Fatal("interrupted recovery accepted")
	}
	m.Observe(Scheduler, true, now) // Out of order evidence cannot resolve incident.
	if !m.Snapshot(Scheduler, now.Add(91*time.Second)).Unresolved {
		t.Fatal("out of order observation changed state")
	}
}

func TestCollectorConcurrentAndReadOnly(t *testing.T) {
	m := &Monitor{}
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(newCollector(m))
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				m.Observe(MetadataWrite, j%2 == 0, time.Now())
				if _, err := registry.Gather(); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	before := m.Snapshot(MetadataWrite, time.Now())
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	if len(families) != 3 {
		t.Fatalf("unexpected metrics count %d", len(families))
	}
	for _, f := range families {
		if len(f.Metric) != 3 {
			t.Fatal("unbounded labels")
		}
	}
	after := m.Snapshot(MetadataWrite, time.Now())
	if before != after {
		t.Fatal("scrape mutated state")
	}
}
