package storageprobe

import (
	"dingospeed/pkg/dependency"
	"testing"
	"time"
)

func TestImpactUsesBlockedEvidenceWithoutStartingIO(t *testing.T) {
	p := &Probe{}
	p.status[1] = status{inFlight: true, timedOut: true}
	now := time.Now()
	p.monitor.Observe(dependency.MetadataWrite, false, now.Add(-3*time.Minute))
	evidence, blocked := p.Evidence(dependency.MetadataWrite, now)
	if !blocked || evidence.State != dependency.Unknown {
		t.Fatal("stale blockage lost")
	}
	r := dependency.AssessStorage(dependency.Snapshot{}, evidence, true, blocked, now)
	if r.Assessment != dependency.ImpactProbeFailure {
		t.Fatalf("lost ongoing blockage: %+v", r)
	}
	if _, blocked := p.Evidence(dependency.Scheduler, now); blocked {
		t.Fatal("scheduler mixed into storage")
	}
}
