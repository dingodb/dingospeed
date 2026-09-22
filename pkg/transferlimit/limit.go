// Package transferlimit bounds active HTTP transfers until their bodies close.
package transferlimit

import (
	"context"
	"io"
	"net/http"
	"sync"
)

type Gate struct {
	mu      sync.Mutex
	active  int
	changed chan struct{}
}

func (g *Gate) Acquire(ctx context.Context, limit func() int) (func(), error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		g.mu.Lock()
		if g.changed == nil {
			g.changed = make(chan struct{})
		}
		if g.active < max(1, limit()) {
			g.active++
			g.mu.Unlock()
			var once sync.Once
			return func() {
				once.Do(func() { g.mu.Lock(); g.active--; close(g.changed); g.changed = make(chan struct{}); g.mu.Unlock() })
			}, nil
		}
		changed := g.changed
		g.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

func (g *Gate) Wake() {
	g.mu.Lock()
	if g.changed != nil {
		close(g.changed)
	}
	g.changed = make(chan struct{})
	g.mu.Unlock()
}

type body struct {
	io.ReadCloser
	release func()
}

func (b *body) Close() error { defer b.release(); return b.ReadCloser.Close() }
func (b *body) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err == io.EOF {
		b.release()
	}
	return n, err
}

func (g *Gate) Do(client *http.Client, req *http.Request, limit func() int) (*http.Response, error) {
	release, err := g.Acquire(req.Context(), limit)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		release()
		return resp, err
	}
	resp.Body = &body{resp.Body, release}
	return resp, nil
}
