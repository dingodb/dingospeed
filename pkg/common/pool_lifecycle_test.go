package common

import (
	"context"
	"errors"
	"testing"
	"time"
)

type lifecycleTask struct {
	id               int
	entered, release chan struct{}
}

func (t *lifecycleTask) GetTaskNo() int                   { return t.id }
func (t *lifecycleTask) GetCancelFun() context.CancelFunc { return func() {} }
func (t *lifecycleTask) DoTask()                          { close(t.entered); <-t.release }
func TestPoolRejectsDuplicateAndCanceledSubmission(t *testing.T) {
	p := NewPool(1, true)
	defer p.Close()
	first := &lifecycleTask{1, make(chan struct{}), make(chan struct{})}
	if err := p.Submit(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	defer close(first.release)
	<-first.entered
	second := &lifecycleTask{1, make(chan struct{}), make(chan struct{})}
	if err := p.Submit(context.Background(), second); !errors.Is(err, ErrTaskActive) {
		t.Fatalf("duplicate: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.Submit(ctx, &lifecycleTask{id: 2}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled: %v", err)
	}
	if p.ActiveCount() != 1 {
		t.Fatal("failed admission leaked a registration")
	}
}
func TestLocalTransferWaitsForClaimedHandler(t *testing.T) {
	transfer := NewLocalTransfer()
	finish, ok := ClaimLocalTransfer(transfer.ID())
	if !ok {
		t.Fatal("claim failed")
	}
	waited := make(chan struct{})
	go func() { transfer.Wait(); close(waited) }()
	select {
	case <-waited:
		t.Fatal("client returned before handler finished")
	default:
	}
	finish()
	select {
	case <-waited:
	case <-time.After(time.Second):
		t.Fatal("completion not delivered")
	}
	if _, ok := ClaimLocalTransfer(transfer.ID()); ok {
		t.Fatal("completed request reclaimed")
	}
	canceled := NewLocalTransfer()
	canceled.Wait()
	if _, ok := ClaimLocalTransfer(canceled.ID()); ok {
		t.Fatal("late request started after cancellation")
	}
}
