// Package storageprobe gathers bounded, independent storage evidence.
// Probe results never change request handling or clear business observations.
package storageprobe

import (
	"context"
	"errors"
	"os"
	"sync"
	"time"

	"dingospeed/pkg/dependency"
	"dingospeed/pkg/util"
)

type Operation func(context.Context) error

var kinds = [...]string{string(util.FileAccessPermissionDenied), string(util.FileAccessMountDisconnected), string(util.FileAccessIOFailure), string(util.FileAccessUnavailable), "not_found", "invalid_sentinel", "timeout"}

type status struct {
	inFlight, timedOut bool
	errors             [len(kinds)]uint64
}

type Probe struct {
	monitor           dependency.Monitor
	mu                sync.Mutex
	status            [2]status
	once              sync.Once
	workers           sync.WaitGroup // Controllers only; blocked filesystem calls are not awaited.
	cancel            context.CancelFunc
	stopped           bool
	interval, timeout time.Duration
	ops               [2]Operation
}

func New(read, write Operation, interval, timeout time.Duration) (*Probe, error) {
	if read == nil || write == nil || timeout <= 0 || interval <= timeout {
		return nil, errors.New("probe requires two operations and interval > timeout > 0")
	}
	return &Probe{interval: interval, timeout: timeout, ops: [2]Operation{read, write}}, nil
}

// Start is one-shot: restarting the controllers cannot replace a stuck worker.
func (p *Probe) Start(ctx context.Context) {
	p.once.Do(func() {
		ctx, cancel := context.WithCancel(ctx)
		p.mu.Lock()
		if p.stopped {
			p.mu.Unlock()
			cancel()
			return
		}
		p.cancel = cancel
		p.workers.Add(2)
		p.mu.Unlock()
		for i := range p.ops {
			go p.run(ctx, i)
		}
	})
}

// Stop stops scheduling immediately; it cannot interrupt an OS filesystem call.
// An already blocked call retains its slot until it returns or the process exits.
func (p *Probe) Stop() {
	p.mu.Lock()
	p.stopped = true
	if p.cancel != nil {
		p.cancel()
	}
	p.mu.Unlock()
	p.workers.Wait()
}

func (p *Probe) run(ctx context.Context, id int) {
	defer p.workers.Done()
	for ctx.Err() == nil {
		opCtx, cancel := context.WithTimeout(ctx, p.timeout)
		result := make(chan error, 1)
		p.mu.Lock()
		p.status[id].inFlight = true
		p.status[id].timedOut = false
		p.mu.Unlock()
		go func() {
			err := p.ops[id](opCtx)
			p.mu.Lock()
			p.status[id].inFlight = false
			p.mu.Unlock()
			result <- err
		}()
		select {
		case err := <-result:
			if ctx.Err() == nil {
				if opCtx.Err() != nil {
					err = opCtx.Err()
				}
				p.observe(id, err)
			}
		case <-opCtx.Done():
			if ctx.Err() != nil {
				cancel()
				return
			}
			p.observe(id, context.DeadlineExceeded)
			// Never launch a replacement while the timed-out operation is alive.
			select {
			case <-result: // A late result cannot count as recovery evidence.
			case <-ctx.Done():
				cancel()
				return
			}
		}
		cancel()
		timer := time.NewTimer(p.interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (p *Probe) observe(id int, err error) {
	if err != nil {
		kind := string(util.FileAccessUnavailable)
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			kind = "timeout"
		case errors.Is(err, errSentinel):
			kind = "invalid_sentinel"
		case errors.Is(err, os.ErrNotExist):
			kind = "not_found"
		default:
			if classified, ok := util.ClassifyFileAccessError("", err); ok {
				kind = string(classified.Kind)
			}
		}
		p.mu.Lock()
		p.status[id].timedOut = kind == "timeout"
		for i, k := range kinds {
			if k == kind {
				p.status[id].errors[i]++
				break
			}
		}
		p.mu.Unlock()
	}
	p.monitor.Observe(dependency.ID(id), err == nil, time.Now())
}

func (p *Probe) snapshot(id int) status {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.status[id]
}

// Evidence reads existing probe observations; it never starts filesystem I/O.
func (p *Probe) Evidence(id dependency.ID, now time.Time) (dependency.Snapshot, bool) {
	if id != dependency.MetadataRead && id != dependency.MetadataWrite {
		return dependency.Snapshot{}, false
	}
	s := p.snapshot(int(id))
	return p.monitor.Snapshot(id, now), s.inFlight && s.timedOut
}
