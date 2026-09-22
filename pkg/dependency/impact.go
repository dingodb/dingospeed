package dependency

import "time"

type Assessment string

const (
	ImpactUnknown                     Assessment = "unknown"
	ImpactHealthy                     Assessment = "healthy_evidence"
	ImpactPartial                     Assessment = "partial_evidence"
	ImpactBusinessFailure             Assessment = "business_failure"
	ImpactBusinessFailureProbeHealthy Assessment = "business_failure_probe_healthy"
	ImpactProbeFailure                Assessment = "probe_failure"
	ImpactCorrelatedFailure           Assessment = "correlated_failure"
	ImpactUncorrelatedFailures        Assessment = "uncorrelated_failures"
	ImpactUnresolved                  Assessment = "unresolved_evidence"
)

var assessments = [...]Assessment{ImpactUnknown, ImpactHealthy, ImpactPartial, ImpactBusinessFailure, ImpactBusinessFailureProbeHealthy, ImpactProbeFailure, ImpactCorrelatedFailure, ImpactUncorrelatedFailures, ImpactUnresolved}

// Impact is evidence for one storage operation, never a whole-node or mount
// diagnosis. Original observations are retained for explanation, not mutated.
type Impact struct {
	Assessment                 Assessment
	Business, Probe            Snapshot
	ProbeEnabled, ProbeBlocked bool
}

// AssessStorage combines passive business evidence and an independent probe.
// The result is deliberately not an HTTP, readiness, retry or restart decision.
func AssessStorage(business, probe Snapshot, enabled, blocked bool, now time.Time) Impact {
	fresh := func(s Snapshot) Snapshot {
		if s.LastObservation.IsZero() || s.LastObservation.After(now) || now.Sub(s.LastObservation) > Freshness {
			s.State = Unknown
		}
		return s
	}
	business = fresh(business)
	if enabled {
		probe = fresh(probe)
	} else {
		probe = Snapshot{}
		blocked = false
	}
	r := Impact{Business: business, Probe: probe, ProbeEnabled: enabled, ProbeBlocked: blocked}
	bad := func(s Snapshot) bool { return s.State == Suspect || s.State == Degraded }
	bBad, pBad := bad(business), bad(probe) || blocked
	bGood, pGood := business.State == ObservedHealthy && !business.Unresolved, probe.State == ObservedHealthy && !probe.Unresolved
	switch {
	case bBad && pBad:
		probeFailure := probe.LastFailure
		if blocked {
			probeFailure = now
		} // Still in flight is current evidence.
		delta := business.LastFailure.Sub(probeFailure)
		if delta < 0 {
			delta = -delta
		}
		r.Assessment = ImpactUncorrelatedFailures
		if !business.LastFailure.IsZero() && !probeFailure.IsZero() && delta <= EvidenceWindow {
			r.Assessment = ImpactCorrelatedFailure
		}
	case bBad:
		r.Assessment = ImpactBusinessFailure
		if pGood && !business.LastFailure.IsZero() && !probe.LastObservation.Before(business.LastFailure) {
			r.Assessment = ImpactBusinessFailureProbeHealthy
		}
	case pBad:
		r.Assessment = ImpactProbeFailure
	case business.Unresolved || probe.Unresolved:
		r.Assessment = ImpactUnresolved
	case bGood && pGood:
		r.Assessment = ImpactHealthy
	case bGood || pGood:
		r.Assessment = ImpactPartial
	default:
		r.Assessment = ImpactUnknown
	}
	return r
}
