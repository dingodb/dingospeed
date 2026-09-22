package common

import (
	"context"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"sync/atomic"
)

type cacheTraceKey struct{}
type CacheTransferTrace struct {
	Requests     atomic.Int64
	Canceled     atomic.Int64
	Active       atomic.Int64
	WrittenBytes atomic.Int64
	stopped      atomic.Bool
}

var cacheSourceCanceled = promauto.NewCounter(prometheus.CounterOpts{Name: "dingospeed_cache_source_cancellations_total", Help: "Source requests canceled by cache tasks"})
var cacheAfterStop = promauto.NewCounterVec(prometheus.CounterOpts{Name: "dingospeed_cache_activity_after_stop_total", Help: "Unexpected cache activity after the completion barrier"}, []string{"operation"})

func WithCacheTrace(ctx context.Context, trace *CacheTransferTrace) context.Context {
	return context.WithValue(ctx, cacheTraceKey{}, trace)
}
func CacheTrace(ctx context.Context) *CacheTransferTrace {
	trace, _ := ctx.Value(cacheTraceKey{}).(*CacheTransferTrace)
	return trace
}
func (trace *CacheTransferTrace) Begin() bool {
	if trace == nil {
		return true
	}
	if trace.stopped.Load() {
		cacheAfterStop.WithLabelValues("request").Inc()
		return false
	}
	trace.Active.Add(1)
	if trace.stopped.Load() {
		trace.Active.Add(-1)
		cacheAfterStop.WithLabelValues("request").Inc()
		return false
	}
	trace.Requests.Add(1)
	return true
}
func (trace *CacheTransferTrace) End(canceled bool) {
	if trace == nil {
		return
	}
	trace.Active.Add(-1)
	if canceled {
		trace.Canceled.Add(1)
		cacheSourceCanceled.Inc()
	}
}
func (trace *CacheTransferTrace) Wrote(bytes int64) {
	if trace == nil {
		return
	}
	trace.WrittenBytes.Add(bytes)
	if trace.stopped.Load() {
		cacheAfterStop.WithLabelValues("write").Inc()
	}
}
func (trace *CacheTransferTrace) Stop() {
	if trace != nil {
		trace.stopped.Store(true)
	}
}
