// Package dependency observes existing operations without controlling requests.
package dependency

import (
	"sync"
	"time"
)

type ID int

const (
	MetadataRead ID = iota
	MetadataWrite
	Scheduler
	dependencyCount
)

type State int

const (
	Unknown State = iota
	ObservedHealthy
	Suspect
	Degraded
	Recovering
)

// These conservative defaults require evidence spread over time, not a burst
// of failed suboperations from a single request. No traffic means unknown.
const (
	EvidenceWindow  = 30 * time.Second
	MinEvidenceSpan = 10 * time.Second
	Freshness       = 2 * time.Minute
)

type evidence struct {
	// At most 31 distinct seconds intersect an inclusive 30-second window.
	times   [31]time.Time
	samples int
}

func (e *evidence) add(now time.Time) {
	start := 0
	for start < e.samples && now.Sub(e.times[start]) > EvidenceWindow {
		start++
	}
	if start > 0 {
		e.samples = copy(e.times[:], e.times[start:e.samples])
	}
	if e.samples > 0 && now.Unix() == e.times[e.samples-1].Unix() {
		return
	}
	e.times[e.samples] = now
	e.samples++
}
func (e evidence) sufficient() bool {
	return e.samples >= 3 && e.times[e.samples-1].Sub(e.times[0]) >= MinEvidenceSpan
}

type Snapshot struct {
	State           State
	LastObservation time.Time
	LastFailure     time.Time
	Unresolved      bool
}
type entry struct {
	Snapshot
	failures, successes evidence
}
type Monitor struct {
	mu      sync.Mutex
	entries [dependencyCount]entry
}

var Default = &Monitor{}

// Observe records a completed operation. Missing files and invalid content must
// be filtered by the caller. Success is positive evidence for this capability
// only; it does not prove every path, mount or other capability is healthy.
func (m *Monitor) Observe(id ID, success bool, now time.Time) {
	if id < 0 || id >= dependencyCount {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	e := &m.entries[id]
	if !e.LastObservation.IsZero() && now.Before(e.LastObservation) {
		return
	}
	e.LastObservation = now
	if !success {
		e.LastFailure = now
		e.Unresolved = true
		e.successes = evidence{}
		e.failures.add(now)
		if e.State != Degraded {
			e.State = Suspect
		}
		if e.failures.sufficient() {
			e.State = Degraded
		}
		return
	}
	if !e.Unresolved {
		e.State = ObservedHealthy
		return
	}
	e.successes.add(now)
	e.State = Recovering
	if e.successes.sufficient() {
		e.State = ObservedHealthy
		e.Unresolved = false
		e.failures = evidence{}
		e.successes = evidence{}
	}
}

// Snapshot does not probe dependencies or modify state. Stale evidence never
// resolves an incident: Unresolved stays true until positive recovery evidence.
func (m *Monitor) Snapshot(id ID, now time.Time) Snapshot {
	if id < 0 || id >= dependencyCount {
		return Snapshot{State: Unknown}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.entries[id].Snapshot
	if s.LastObservation.IsZero() || now.Sub(s.LastObservation) > Freshness {
		s.State = Unknown
	}
	return s
}
