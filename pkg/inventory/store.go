package inventory

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

// Barrier excludes effective metadata mutations while a snapshot is fixed.
// Network calls never hold it. Durable state lives outside every repository.
var Barrier sync.RWMutex
var storeMu sync.Mutex
var Wake = make(chan struct{}, 1)
var ErrDamaged = errors.New("inventory state damaged")

func Signal() {
	select {
	case Wake <- struct{}{}:
	default:
	}
}

type Operation struct {
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
}
type Entry struct {
	Key
	Sequence  uint64     `json:"sequence"`
	Deleted   bool       `json:"deleted"`
	Operation *Operation `json:"operation,omitempty"`
	Report    *Report    `json:"report,omitempty"`
	Due       time.Time  `json:"due"`
	Attempts  int        `json:"attempts"`
	Error     string     `json:"error,omitempty"`
}
type State struct {
	BuildAttempts  int               `json:"buildAttempts,omitempty"`
	BuildBlocked   bool              `json:"buildBlocked,omitempty"`
	Version        int               `json:"version"`
	NodeID         string            `json:"nodeId"`
	Epoch          string            `json:"epoch"`
	Sequence       uint64            `json:"sequence"`
	Entries        map[string]*Entry `json:"entries"`
	ReconcileEpoch string            `json:"reconcileEpoch,omitempty"`
	Baseline       *Report           `json:"baseline,omitempty"`
	Cutoff         uint64            `json:"cutoff"`
	RetryAt        time.Time         `json:"retryAt"`
	Attempts       int               `json:"attempts"`
	Error          string            `json:"error,omitempty"`
}

func statePath(root string) string { return filepath.Join(root, ".upload-inventory", "outbox.json") }
func load(root string) (*State, error) {
	b, e := os.ReadFile(statePath(root))
	if os.IsNotExist(e) {
		return &State{Version: 2, Entries: map[string]*Entry{}}, nil
	}
	if e != nil {
		return nil, e
	}
	var s State
	if e = json.Unmarshal(b, &s); e != nil {
		return nil, fmt.Errorf("%w; explicit recovery required: %v", ErrDamaged, e)
	}
	if s.Version != 2 || s.Entries == nil {
		return nil, ErrDamaged
	}
	return &s, nil
}
func Read(root string) (*State, error) { storeMu.Lock(); defer storeMu.Unlock(); return load(root) }
func Update(root string, fn func(*State) error) error {
	storeMu.Lock()
	defer storeMu.Unlock()
	s, e := load(root)
	if e != nil {
		return e
	}
	before, e := json.Marshal(s)
	if e != nil {
		return e
	}
	if e = fn(s); e != nil {
		return e
	}
	b, e := json.Marshal(s)
	if e != nil {
		return e
	}
	if bytes.Equal(before, b) {
		return nil
	}
	return AtomicWrite(statePath(root), b)
}
func AtomicWrite(path string, b []byte) error {
	dir := filepath.Dir(path)
	if e := os.MkdirAll(dir, 0700); e != nil {
		return e
	}
	f, e := os.CreateTemp(dir, ".inventory-*")
	if e != nil {
		return e
	}
	name := f.Name()
	defer os.Remove(name)
	if _, e = f.Write(b); e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	if e = os.Rename(name, path); e != nil {
		return e
	}
	return SyncParents(filepath.Dir(dir), path)
}

// SyncParents makes newly created directories and renamed/deleted entries
// durable before their write-ahead operation is acknowledged locally.
func SyncParents(root, path string) error {
	if runtime.GOOS == "windows" {
		return nil
	} // directory fsync is unavailable here
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	dir, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return err
	}
	for {
		rel, err := filepath.Rel(root, dir)
		if err != nil || rel == ".." || len(rel) > 3 && rel[:3] == ".."+string(filepath.Separator) {
			return errors.New("inventory sync outside repository root")
		}
		f, err := os.Open(dir)
		if err == nil {
			err = f.Sync()
			_ = f.Close()
			if err != nil {
				return err
			}
		} else if !os.IsNotExist(err) {
			return err
		}
		if dir == root {
			return nil
		}
		dir = filepath.Dir(dir)
	}
}

// Run journals before the first effective change. A failed completion write leaves
// a replayable operation, not an unrecorded successful local mutation.
func Run(root string, k Key, kind string, data any, fn func() error) error {
	Barrier.RLock()
	defer Barrier.RUnlock()
	b, e := json.Marshal(data)
	if e != nil {
		return e
	}
	e = Update(root, func(s *State) error {
		old := s.Entries[k.ID()]
		if old != nil && old.Operation != nil {
			return errors.New("repository operation awaits recovery")
		}
		if s.Sequence == ^uint64(0) {
			return errors.New("inventory sequence exhausted")
		}
		s.Sequence++
		if old == nil {
			old = &Entry{Key: k}
			s.Entries[k.ID()] = old
		}
		old.Sequence = s.Sequence
		old.Operation = &Operation{Kind: kind, Data: b}
		// The first dirty deadline is not pushed back by continuous mutations.
		if old.Due.IsZero() {
			old.Due = time.Now().Add(300 * time.Millisecond)
		}
		return nil
	})
	if e != nil {
		return e
	}
	defer Signal()
	if e = fn(); e != nil {
		return e
	}
	// The business effect is already committed. A failed journal cleanup is retried.
	_ = Finish(root, k, kind)
	return nil
}
func Finish(root string, k Key, kind string) error {
	return Update(root, func(s *State) error {
		e := s.Entries[k.ID()]
		if e == nil {
			return errors.New("missing operation record")
		}
		e.Operation = nil
		e.Deleted = kind == "delete-repository"
		e.Error = ""
		return nil
	})
}
func RequestReconcile(root, epoch string) error {
	if epoch == "" {
		return errors.New("missing reconciliation epoch")
	}
	err := Update(root, func(s *State) error {
		if s.Epoch == epoch {
			return nil
		}
		if s.ReconcileEpoch != epoch {
			s.ReconcileEpoch = epoch
			s.Baseline = nil
			s.BuildAttempts = 0
			s.BuildBlocked = false
			s.Attempts = 0
			s.RetryAt = time.Time{}
			s.Error = ""
		}
		return nil
	})
	if errors.Is(err, ErrDamaged) {
		// Only an explicit request can replace corrupt state. Preserve its bytes
		// for investigation instead of silently treating corruption as an empty queue.
		storeMu.Lock()
		old, readErr := os.ReadFile(statePath(root))
		if readErr == nil {
			backup := statePath(root) + ".damaged-" + time.Now().UTC().Format("20060102T150405.000000000")
			readErr = AtomicWrite(backup, old)
		}
		if readErr == nil {
			b, _ := json.Marshal(&State{Version: 2, Entries: map[string]*Entry{}, ReconcileEpoch: epoch})
			readErr = AtomicWrite(statePath(root), b)
		}
		storeMu.Unlock()
		err = readErr
	}
	if err == nil {
		Signal()
	}
	return err
}

// Confirm consumes exactly the report acknowledged. New mutations survive.
func Confirm(root string, p *Report, a Ack) error {
	if a.Epoch != p.Epoch || a.Sequence != p.Sequence || a.Digest != p.Digest() || (a.Status != "accepted" && a.Status != "confirmed" && a.Status != "obsolete") {
		return errors.New("inventory acknowledgement mismatch")
	}
	return Update(root, func(s *State) error {
		if p.Baseline {
			if s.Baseline == nil || s.Baseline.Digest() != p.Digest() {
				return errors.New("baseline changed")
			}
			// Epoch fencing makes reset safe. Rebase post-snapshot changes only.
			cutoff := s.Cutoff
			s.Sequence -= cutoff
			for id, e := range s.Entries {
				if e.Sequence <= cutoff {
					delete(s.Entries, id)
				} else {
					e.Sequence -= cutoff
					e.Report = nil
					e.Attempts = 0
					e.Due = time.Now()
				}
			}
			s.Epoch = p.Epoch
			s.BuildAttempts = 0
			s.BuildBlocked = false
			s.Baseline = nil
			s.ReconcileEpoch = ""
			s.Cutoff = 0
			s.Attempts = 0
			s.RetryAt = time.Time{}
			s.Error = ""
			return nil
		}
		e := s.Entries[p.Key.ID()]
		if e == nil || e.Report == nil || e.Report.Digest() != p.Digest() {
			return errors.New("pending report changed")
		}
		if e.Sequence == p.Sequence && e.Operation == nil {
			delete(s.Entries, p.Key.ID())
		} else {
			e.Report = nil
			e.Attempts = 0
			e.Due = time.Now()
		}
		return nil
	})
}
