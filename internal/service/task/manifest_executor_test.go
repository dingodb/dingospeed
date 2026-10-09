package task

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

func TestExecuteManifestJoinsTransfersOnPreparationFailure(t *testing.T) {
	started := make(chan struct{})
	var stopped atomic.Bool
	want := errors.New("metadata failure")
	err := ExecuteManifest(context.Background(), 2, 2, func(ctx context.Context, i int) (func(context.Context) error, error) {
		if i == 1 {
			<-started
			return nil, want
		}
		return func(ctx context.Context) error { close(started); <-ctx.Done(); stopped.Store(true); return ctx.Err() }, nil
	})
	if !errors.Is(err, want) || !stopped.Load() {
		t.Fatalf("err=%v joined=%t", err, stopped.Load())
	}
}
