package common

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
)

// LocalTransfer joins a loopback handler, not merely its HTTP client. A canceled
// client must still wait for the handler's cache producers to release their files.
const LocalTransferHeader = "X-Dingo-Local-Transfer"

var localTransfers sync.Map

type LocalTransfer struct {
	Trace           *CacheTransferTrace
	mu              sync.Mutex
	claimed, closed bool
	done            chan struct{}
	id              string
}

func NewLocalTransfer(traces ...*CacheTransferTrace) *LocalTransfer {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	t := &LocalTransfer{id: hex.EncodeToString(b[:]), done: make(chan struct{})}
	if len(traces) > 0 {
		t.Trace = traces[0]
	}
	localTransfers.Store(t.id, t)
	return t
}
func (t *LocalTransfer) ID() string { return t.id }
func ClaimLocalTransfer(id string) (func(), bool) {
	value, ok := localTransfers.Load(id)
	if !ok {
		return nil, false
	}
	t := value.(*LocalTransfer)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || t.claimed {
		return nil, false
	}
	t.claimed = true
	return func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		if !t.closed {
			t.closed = true
			close(t.done)
		}
	}, true
}
func (t *LocalTransfer) Wait() {
	t.mu.Lock()
	if !t.claimed && !t.closed {
		t.closed = true
		close(t.done)
	}
	t.mu.Unlock()
	<-t.done
	localTransfers.Delete(t.id)
}

func LocalCacheTrace(id string) *CacheTransferTrace {
	if value, ok := localTransfers.Load(id); ok {
		return value.(*LocalTransfer).Trace
	}
	return nil
}
