package task

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	myerr "dingospeed/pkg/error"
)

func fastPreheatRetries(t *testing.T, n int) {
	t.Helper()
	old := PreheatRetryBackoff
	t.Cleanup(func() { PreheatRetryBackoff = old })
	PreheatRetryBackoff = make([]time.Duration, n)
	for i := range PreheatRetryBackoff {
		PreheatRetryBackoff[i] = time.Millisecond
	}
}

// 2026-10-13: a burst of dropped connections failed a 1.7 TB job at 87% and a
// single metadata timeout failed CulturaX; both only needed another pass.
func TestPreheatRetriesTransientFailures(t *testing.T) {
	fastPreheatRetries(t, 3)
	p := preheatFixture(t, 3)
	var calls, failures atomic.Int32
	p.transfer = func(_, _, fileName, _, _, _ string, _, _ int64) error {
		calls.Add(1)
		if fileName == "shard-01.bin" && failures.Add(1) <= 2 {
			return io.ErrUnexpectedEOF
		}
		return nil
	}
	if err := p.preheatWithRetry("huggingface/owner/model"); err != nil {
		t.Fatalf("transient failures not retried: %v", err)
	}
	if failures.Load() != 3 {
		t.Fatalf("failing file attempted %d times, want 3", failures.Load())
	}
}

func TestPreheatRetryGivesUp(t *testing.T) {
	cases := map[string]struct {
		err      error
		attempts int32
		cancel   bool
	}{
		"exhausted":   {io.ErrUnexpectedEOF, 3, false},
		"forbidden":   {myerr.NewAppendCode(http.StatusForbidden, "gated"), 1, false},
		"not found":   {myerr.WrapCode(http.StatusNotFound, "gone", errors.New("404")), 1, false},
		"job stopped": {io.ErrUnexpectedEOF, 1, true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			fastPreheatRetries(t, 2)
			p := preheatFixture(t, 1)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			p.Ctx = ctx
			var calls atomic.Int32
			p.transfer = func(_, _, _, _, _, _ string, _, _ int64) error {
				calls.Add(1)
				if c.cancel {
					cancel()
				}
				return c.err
			}
			if err := p.preheatWithRetry("huggingface/owner/model"); err == nil {
				t.Fatal("failure swallowed")
			}
			if calls.Load() != c.attempts {
				t.Fatalf("attempted %d times, want %d", calls.Load(), c.attempts)
			}
		})
	}
}
