package prom

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	RequestTotalCnt = promauto.NewCounter(prometheus.CounterOpts{
		Name: "request_total_cnt",
		Help: "Total number of request total",
	})
	RequestDoneCnt = promauto.NewCounter(prometheus.CounterOpts{
		Name: "request_done_cnt",
		Help: "Total number of request done",
	})
	RequestTooManyCnt = promauto.NewCounter(prometheus.CounterOpts{
		Name: "request_too_many_cnt",
		Help: "Total number of request too many",
	})

	RequestModelCnt = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "request_model_cnt",
		Help: "Total number of request model",
	}, []string{"models", "status"})

	RequestDataSetCnt = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "request_dataset_cnt",
		Help: "Total number of request dataset",
	}, []string{"datasets", "status"})
)
