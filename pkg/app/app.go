package app

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

type AppInfo interface {
	ID() string
	Name() string
	Version() string
	StartTime() string
	Ctx() context.Context
}

type App struct {
	opts   options
	ctx    context.Context
	cancel func()
}

func New(opts ...Option) *App {
	o := options{
		ctx:         context.Background(), // 全局最基础的ctx
		sigs:        []os.Signal{syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGINT},
		stopTimeout: 10 * time.Second,
		startTime:   time.Now().Format(time.DateTime),
	}
	for _, opt := range opts {
		opt(&o)
	}
	ctx, cancel := context.WithCancel(o.ctx)
	return &App{
		opts:   o,
		ctx:    ctx,
		cancel: cancel,
	}
}

func (a *App) ID() string { return a.opts.id }

func (a *App) Name() string { return a.opts.name }

func (a *App) Version() string { return a.opts.version }

func (a *App) StartTime() string { return a.opts.startTime }

func (a *App) Ctx() context.Context { return a.ctx }

func (a *App) Stop() (err error) {
	if a.cancel != nil {
		a.cancel()
	}
	time.Sleep(3 * time.Second)
	ctx, cancel := context.WithTimeout(a.ctx, a.opts.stopTimeout)
	defer cancel()
	for _, srv := range a.opts.servers {
		srv := srv
		if err = srv.Stop(ctx); err != nil {
			zap.S().Errorf("app stop err.%v", err)
		}
	}
	return nil
}

func (a *App) Run() error {
	stx := NewContext(a.ctx, a)
	eg, ctx := errgroup.WithContext(stx)
	wg := sync.WaitGroup{}
	for _, srv := range a.opts.servers {
		srv := srv
		wg.Add(1)
		eg.Go(func() error {
			wg.Done() // here is to ensure server start has begun running before register, so defer is not needed
			return srv.Start(stx)
		})
	}
	wg.Wait()

	c := make(chan os.Signal, 1)
	signal.Notify(c, a.opts.sigs...)
	eg.Go(func() error {
		select {
		case <-ctx.Done():
			return nil
		case <-c:
			return a.Stop()
		}
	})

	if err := eg.Wait(); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
