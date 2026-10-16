package task

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func fastMountRetries(t *testing.T, n int) {
	t.Helper()
	old := MountRetryBackoff
	t.Cleanup(func() { MountRetryBackoff = old })
	MountRetryBackoff = make([]time.Duration, n)
	for i := range MountRetryBackoff {
		MountRetryBackoff[i] = time.Millisecond
	}
}

// failingCmd returns a command factory whose first `failures` commands exit 1.
func failingCmd(failures int, calls *int) func() *exec.Cmd {
	return func() *exec.Cmd {
		*calls++
		if *calls <= failures {
			return exec.Command("sh", "-c", "exit 1")
		}
		return exec.Command("sh", "-c", "exit 0")
	}
}

// A paginated listing that hits one failed lookup midway used to fail the
// whole copy; a rerun only needs to pick up where the last one stopped.
func TestMountRetriesTransientFailures(t *testing.T) {
	fastMountRetries(t, 3)
	var calls int
	var log bytes.Buffer
	if err := runMountWithRetry(context.Background(), "owner/repo", failingCmd(2, &calls), &log); err != nil {
		t.Fatalf("transient failures not retried: %v", err)
	}
	if calls != 3 {
		t.Fatalf("expected 3 runs, got %d", calls)
	}
	if got := strings.Count(log.String(), "retry in"); got != 2 {
		t.Fatalf("expected 2 retry markers in log, got %d:\n%s", got, log.String())
	}
}

func TestMountRetryGivesUpAfterBackoff(t *testing.T) {
	fastMountRetries(t, 2)
	var calls int
	if err := runMountWithRetry(context.Background(), "owner/repo", failingCmd(100, &calls), &bytes.Buffer{}); err == nil {
		t.Fatal("expected error after retries are exhausted")
	}
	if calls != 3 {
		t.Fatalf("expected 1 run + 2 retries, got %d", calls)
	}
}

func TestMountRetryStopsWhenCancelled(t *testing.T) {
	fastMountRetries(t, 3)
	ctx, cancel := context.WithCancel(context.Background())
	var calls int
	newCmd := func() *exec.Cmd {
		calls++
		cancel()
		return exec.Command("sh", "-c", "exit 1")
	}
	if err := runMountWithRetry(ctx, "owner/repo", newCmd, &bytes.Buffer{}); err == nil {
		t.Fatal("expected error")
	}
	if calls != 1 {
		t.Fatalf("cancelled job retried: %d runs", calls)
	}
}

func TestMountRetryDisabled(t *testing.T) {
	fastMountRetries(t, 0)
	var calls int
	if err := runMountWithRetry(context.Background(), "owner/repo", failingCmd(1, &calls), &bytes.Buffer{}); err == nil {
		t.Fatal("expected error with retries disabled")
	}
	if calls != 1 {
		t.Fatalf("expected a single run, got %d", calls)
	}
}
