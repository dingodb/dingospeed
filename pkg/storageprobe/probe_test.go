package storageprobe

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"dingospeed/pkg/dependency"
	"github.com/prometheus/client_golang/prometheus"
)

func await(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition timed out")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestBlockedCallHasOneSlotAndStopDoesNotWaitForIO(t *testing.T) {
	var reads, writes atomic.Int32
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	p, err := New(func(context.Context) error { reads.Add(1); <-release; return nil }, func(context.Context) error { writes.Add(1); return nil }, 30*time.Millisecond, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	p.Start(context.Background())
	defer p.Stop()
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(Collector(p))
	await(t, func() bool { return writes.Load() >= 4 })
	for i := 0; i < 20; i++ {
		if _, err := registry.Gather(); err != nil {
			t.Fatal(err)
		}
	}
	s := p.snapshot(0)
	if reads.Load() != 1 || !s.inFlight || !s.timedOut || s.errors[len(kinds)-1] != 1 {
		t.Fatalf("blocked operation replaced or counted repeatedly: reads=%d status=%+v", reads.Load(), s)
	}
	if got := p.monitor.Snapshot(dependency.MetadataRead, time.Now()); !got.Unresolved {
		t.Fatal("timeout unobserved")
	}
	if got := p.monitor.Snapshot(dependency.MetadataWrite, time.Now()); got.Unresolved || got.State != dependency.ObservedHealthy {
		t.Fatal("read blockage affected write evidence")
	}
	stopped := make(chan struct{})
	go func() { p.Stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("Stop waited for blocked I/O")
	}
	unblock()
	await(t, func() bool { return !p.snapshot(0).inFlight })
	if !p.monitor.Snapshot(dependency.MetadataRead, time.Now()).Unresolved {
		t.Fatal("late result cleared timeout")
	}
	p.Start(context.Background())
	if reads.Load() != 1 {
		t.Fatal("restarted stopped controller")
	}
}

func TestStopBeforeStart(t *testing.T) {
	op := func(context.Context) error { t.Error("operation ran after stop"); return nil }
	p, _ := New(op, op, time.Second, time.Millisecond)
	p.Stop()
	p.Start(context.Background())
	p.Stop()
}

func TestProbeEvidenceCannotClearBusinessFailure(t *testing.T) {
	now := time.Now()
	dependency.Default.Observe(dependency.MetadataRead, false, now)
	before := dependency.Default.Snapshot(dependency.MetadataRead, now)
	p := &Probe{}
	for i := 0; i < 3; i++ {
		p.monitor.Observe(dependency.MetadataRead, true, now.Add(time.Duration(i)*5*time.Second))
	}
	if got := dependency.Default.Snapshot(dependency.MetadataRead, now); got != before {
		t.Fatal("probe mutated business evidence")
	}
}

func TestFaultClassification(t *testing.T) {
	cases := []struct {
		name  string
		err   error
		index int
	}{
		{"permission", &os.PathError{Op: "open", Path: "private", Err: os.ErrPermission}, 0},
		{"missing", &os.PathError{Op: "open", Path: "sentinel", Err: os.ErrNotExist}, 4},
		{"invalid", errSentinel, 5},
		{"deadline", context.DeadlineExceeded, 6},
	}
	if runtime.GOOS == "linux" {
		cases = append(cases, struct {
			name  string
			err   error
			index int
		}{"io", &os.PathError{Op: "read", Err: syscall.Errno(5)}, 2}, struct {
			name  string
			err   error
			index int
		}{"disconnected", &os.PathError{Op: "read", Err: syscall.Errno(107)}, 1})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &Probe{}
			p.observe(0, tc.err)
			if p.snapshot(0).errors[tc.index] != 1 || !p.monitor.Snapshot(dependency.MetadataRead, time.Now()).Unresolved {
				t.Fatal("fault not classified")
			}
		})
	}
}

func provision(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, Directory)
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sentinel"), []byte(Sentinel), 0600); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestFilesystemChecksAndCleanup(t *testing.T) {
	root := provision(t)
	read, write := Filesystem(root)
	if err := read(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := write(context.Background()); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(root, Directory))
	if err != nil || len(entries) != 1 || entries[0].Name() != "sentinel" {
		t.Fatalf("probe file leaked: %v %v", entries, err)
	}
	if err := os.WriteFile(filepath.Join(root, Directory, "sentinel"), []byte("wrong"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := write(context.Background()); !errors.Is(err, errSentinel) {
		t.Fatalf("invalid sentinel accepted: %v", err)
	}
	empty := t.TempDir()
	_, write = Filesystem(empty)
	if err := write(context.Background()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing probe root accepted: %v", err)
	}
	entries, err = os.ReadDir(empty)
	if err != nil || len(entries) != 0 {
		t.Fatal("probe created absent root")
	}
}

func TestCancelledProbeDoesNoIO(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	read, write := Filesystem(filepath.Join(t.TempDir(), "absent"))
	for _, op := range []Operation{read, write} {
		if err := op(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("ignored cancellation: %v", err)
		}
	}
}
