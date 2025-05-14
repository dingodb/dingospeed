package prom

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// 请求总数

	RequestTotalCnt = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "request_total_cnt",
		Help: "Total number of request total",
	}, []string{"source"})
	// 请求成功数

	RequestSuccessCnt = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "request_success_cnt",
		Help: "Total number of request done",
	}, []string{"source"})
	// 请求失败数，客户端或服务端主动端口或程序异常

	RequestFailCnt = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "request_fail_cnt",
		Help: "Total number of request fail",
	}, []string{"source"})

	// 请求过多，被拒绝

	RequestTooManyCnt = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "request_too_many_cnt",
		Help: "Total number of request too many",
	}, []string{"source"})

	// 模型统计

	RequestModelCnt = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "request_model_cnt",
		Help: "Total number of request model",
	}, []string{"models", "source"})

	RequestDataSetCnt = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "request_dataset_cnt",
		Help: "Total number of request dataset",
	}, []string{"datasets", "source"})

	// 流量统计

	RequestRemoteByte = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "request_remote_byte",
		Help: "Total number of request remote byte",
	}, []string{"source"})

	RequestResponseByte = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "request_response_byte",
		Help: "Total number of request response byte",
	}, []string{"source"})
)

func PromSourceCounter(vec *prometheus.GaugeVec, source string) {
	labels := prometheus.Labels{}
	labels["source"] = source
	vec.With(labels).Inc()
}

func PromRequestByteCounter(vec *prometheus.CounterVec, source string, len int64) {
	labels := prometheus.Labels{}
	labels["source"] = source
	vec.With(labels).Add(float64(len))
}
