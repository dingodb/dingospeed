package app

import (
	"context"
	"os"
	"time"

	"dingospeed/pkg/server"
)

type Option func(o *options)

type options struct {
	id          string
	name        string
	version     string
	startTime   string
	ctx         context.Context
	sigs        []os.Signal
	stopTimeout time.Duration
	servers     []server.Server
}

func ID(id string) Option {
	return func(o *options) { o.id = id }
}

func Name(name string) Option {
	return func(o *options) { o.name = name }
}

func Version(version string) Option {
	return func(o *options) {
		o.version = version
	}
}

func Server(srv ...server.Server) Option {
	return func(o *options) { o.servers = srv }
}

func Context(ctx context.Context) Option {
	return func(o *options) { o.ctx = ctx }
}

func StopTimeout(t time.Duration) Option {
	return func(o *options) { o.stopTimeout = t }
}

func Signal(sigs ...os.Signal) Option {
	return func(o *options) { o.sigs = sigs }
}
