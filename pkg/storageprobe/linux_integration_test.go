//go:build linux

package storageprobe

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"dingospeed/pkg/dependency"
	"github.com/prometheus/client_golang/prometheus"
)

// Only the isolated runner supplies these paths; normal go test skips real mounts.
func TestLocalLinuxFilesystemFaults(t *testing.T) {
	root := os.Getenv("DINGOSPEED_PROBE_TEST_ROOT")
	if root == "" {
		t.Skip("run test/storage-probe/run-local.sh in an isolated Linux namespace")
	}
	if os.Geteuid() == 0 {
		t.Fatal("permission tests must run as an unprivileged user")
	}
	control := os.Getenv("DINGOSPEED_PROBE_TEST_CONTROL")
	backing := os.Getenv("DINGOSPEED_PROBE_TEST_BACKING")
	if control == "" || backing == "" {
		t.Fatal("incomplete test environment")
	}
	setMode := func(mode string) {
		t.Helper()
		if err := os.WriteFile(control, []byte(mode), 0600); err != nil {
			t.Fatal(err)
		}
	}
	read, write := Filesystem(root)
	ctx := context.Background()
	t.Run("normal_read_write_sync_cleanup", func(t *testing.T) {
		setMode("normal")
		for _, op := range []Operation{read, write} {
			if err := op(ctx); err != nil {
				t.Fatal(err)
			}
		}
		entries, err := os.ReadDir(filepath.Join(backing, Directory))
		if err != nil || len(entries) != 1 {
			t.Fatalf("cleanup: %v %v", entries, err)
		}
	})
	t.Run("readable_but_not_writable", func(t *testing.T) {
		dir := filepath.Join(backing, Directory)
		if err := os.Chmod(dir, 0500); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(dir, 0700)
		if err := read(ctx); err != nil {
			t.Fatalf("read affected by write-only restriction: %v", err)
		}
		err := write(ctx)
		if !errors.Is(err, syscall.EACCES) {
			t.Fatalf("expected actual EACCES, got %v", err)
		}
		p := &Probe{}
		p.observe(1, err)
		if p.snapshot(1).errors[0] != 1 || p.monitor.Snapshot(dependency.MetadataRead, time.Now()).Unresolved {
			t.Fatal("permission classification/scope")
		}
		t.Logf("uid=%d write errno=%v; read passed", os.Geteuid(), err)
	})
	t.Run("read_permission_denied", func(t *testing.T) {
		sentinel := filepath.Join(backing, Directory, "sentinel")
		if err := os.Chmod(sentinel, 0000); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(sentinel, 0600)
		if err := read(ctx); !errors.Is(err, syscall.EACCES) {
			t.Fatalf("expected actual EACCES: %v", err)
		}
	})
	for _, mode := range []string{"eio-read", "eio-write", "eio-sync"} {
		t.Run(mode, func(t *testing.T) {
			setMode(mode)
			defer setMode("normal")
			op := write
			if mode == "eio-read" {
				op = read
			}
			err := op(ctx)
			if !errors.Is(err, syscall.EIO) {
				t.Fatalf("expected actual EIO: %v", err)
			}
			p := &Probe{}
			p.observe(0, err)
			if p.snapshot(0).errors[2] != 1 {
				t.Fatal("EIO not classified")
			}
			t.Logf("kernel returned %v", err)
		})
	}
	t.Run("failed_cleanup_does_not_accumulate_files", func(t *testing.T) {
		setMode("eio-unlink")
		defer setMode("normal")
		for i := 0; i < 5; i++ {
			if err := write(ctx); !errors.Is(err, syscall.EIO) {
				t.Fatalf("cleanup error lost: %v", err)
			}
			entries, err := os.ReadDir(filepath.Join(backing, Directory))
			if err != nil || len(entries) != 2 {
				t.Fatalf("temporary files accumulated: %v %v", entries, err)
			}
		}
		setMode("normal")
		if err := write(ctx); err != nil {
			t.Fatalf("retry cleanup failed: %v", err)
		}
		entries, _ := os.ReadDir(filepath.Join(backing, Directory))
		if len(entries) != 1 {
			t.Fatal("recovery left temporary files")
		}
	})
	t.Run("blocked_write_real_syscall_is_bounded", func(t *testing.T) {
		setMode("block-write")
		defer setMode("normal")
		p, err := New(read, write, 10*time.Second, 3*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		p.Start(ctx)
		defer p.Stop()
		registry := prometheus.NewPedanticRegistry()
		registry.MustRegister(Collector(p))
		deadline := time.Now().Add(6 * time.Second)
		for s := p.snapshot(1); !s.inFlight || !s.timedOut; s = p.snapshot(1) {
			if time.Now().After(deadline) {
				t.Fatal("default probe timeout was not observed")
			}
			time.Sleep(10 * time.Millisecond)
		}
		before := p.snapshot(1)
		// Observe several would-be retry intervals while the kernel call remains pending.
		for i := 0; i < 32; i++ {
			families, err := registry.Gather()
			if err != nil || len(families) != 4 {
				t.Fatalf("metrics blocked: %v", err)
			}
			entries, err := os.ReadDir(filepath.Join(backing, Directory))
			if err != nil || len(entries) != 2 {
				t.Fatalf("blocked write tasks accumulated temporary files: %v %v", entries, err)
			}
			time.Sleep(time.Second)
		}
		after := p.snapshot(1)
		if after != before || after.errors[6] != 1 {
			t.Fatalf("blocked work retried: before=%+v after=%+v", before, after)
		}
		if s := p.monitor.Snapshot(dependency.MetadataRead, time.Now()); s.State != dependency.ObservedHealthy {
			t.Fatalf("write blockage affected reads: %+v", s)
		}
		start := time.Now()
		p.Stop()
		stopDuration := time.Since(start)
		if stopDuration > time.Second {
			t.Fatal("shutdown blocked on filesystem")
		}
		failed := p.monitor.Snapshot(dependency.MetadataWrite, time.Now())
		setMode("normal")
		await(t, func() bool { return !p.snapshot(1).inFlight })
		if got := p.monitor.Snapshot(dependency.MetadataWrite, time.Now()); got != failed {
			t.Fatalf("late success altered evidence: %+v -> %+v", failed, got)
		}
		t.Logf("default 3s timeout, 10s interval: blocked for >35s, one timeout and one temporary file; read and metrics stayed available; stop=%s", stopDuration)
	})
	t.Run("real_time_positive_recovery", func(t *testing.T) {
		setMode("eio-write")
		p, err := New(read, write, 5*time.Second, 3*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		p.Start(ctx)
		defer p.Stop()
		defer setMode("normal")
		deadline := time.Now().Add(16 * time.Second)
		for p.monitor.Snapshot(dependency.MetadataWrite, time.Now()).State != dependency.Degraded {
			if time.Now().After(deadline) {
				t.Fatal("ongoing EIO never degraded")
			}
			time.Sleep(50 * time.Millisecond)
		}
		setMode("normal")
		deadline = time.Now().Add(17 * time.Second)
		for p.monitor.Snapshot(dependency.MetadataWrite, time.Now()).Unresolved {
			if time.Now().After(deadline) {
				t.Fatal("positive evidence never recovered")
			}
			time.Sleep(50 * time.Millisecond)
		}
		if s := p.monitor.Snapshot(dependency.MetadataWrite, time.Now()); s.State != dependency.ObservedHealthy {
			t.Fatalf("unexpected recovery %+v", s)
		}
	})
	// Last: a separate root controller kills only this disposable FUSE daemon.
	t.Run("host_remount_healthy_but_old_namespace_ENOTCONN", func(t *testing.T) {
		request := os.Getenv("DINGOSPEED_PROBE_TEST_DISCONNECT")
		if request == "" {
			t.Fatal("missing disconnect controller")
		}
		if err := os.WriteFile(request, []byte("disconnect"), 0600); err != nil {
			t.Fatal(err)
		}
		await(t, func() bool {
			data, err := os.ReadFile(request + ".healthy")
			return err == nil && string(data) == Sentinel
		})
		deadline := time.Now().Add(3 * time.Second)
		var err error
		for time.Now().Before(deadline) {
			err = read(ctx)
			if errors.Is(err, syscall.ENOTCONN) {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if !errors.Is(err, syscall.ENOTCONN) {
			t.Fatalf("expected real ENOTCONN after daemon exit: %v", err)
		}
		p := &Probe{}
		p.observe(0, err)
		if p.snapshot(0).errors[1] != 1 {
			t.Fatal("disconnected mount not classified")
		}
		if writeErr := write(ctx); !errors.Is(writeErr, syscall.ENOTCONN) {
			t.Fatalf("old namespace write unexpectedly usable: %v", writeErr)
		}
		t.Logf("parent namespace remount read succeeded as uid 65534; old namespace returned %v", err)
	})
}

func TestLocalNetworkIsolation(t *testing.T) {
	if os.Getenv("DINGOSPEED_PROBE_TEST_ROOT") == "" {
		t.Skip("local runner only")
	}
	data, err := os.ReadFile("/proc/net/route")
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(data))
	if len(fields) != 0 && (len(fields) != 11 || fields[0] != "Iface") {
		t.Fatalf("unexpected IPv4 route table: %s", data)
	}
	entries, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name != "lo" || entry.Flags&net.FlagUp != 0 {
			t.Fatalf("external or active interface present: %s", entry.Name)
		}
	}
	if fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, 0); err != nil {
		t.Fatal(err)
	} else {
		syscall.Close(fd)
	}
	t.Log("no external interfaces or IPv4 routes; unprivileged test uid=" + strconv.Itoa(os.Geteuid()))
}
