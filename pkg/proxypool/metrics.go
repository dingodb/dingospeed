//  Copyright (c) 2025 dingodb.com, Inc. All Rights Reserved
//
//  Licensed under the Apache License, Version 2.0 (the "License");
//  you may not use this file except in compliance with the License.
//  You may obtain a copy of the License at
//
//      http:www.apache.org/licenses/LICENSE-2.0
//
//  Unless required by applicable law or agreed to in writing, software
//  distributed under the License is distributed on an "AS IS" BASIS,
//  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//  See the License for the specific language governing permissions and
//  limitations under the License.

package proxypool

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// MemberHealthy 每个出口的健康状态，1=可用 0=熔断中。
	MemberHealthy = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "proxypool_member_healthy",
		Help: "Proxy pool member health, 1 healthy 0 tripped",
	}, []string{"member"})

	// MemberRequestTotal 按出口与结果统计请求数。
	MemberRequestTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "proxypool_member_request_total",
		Help: "Total requests routed through each proxy pool member",
	}, []string{"member", "result"})

	// AvailableMembers 当前可用出口数，掉到 0 意味着已全部回退直连。
	AvailableMembers = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "proxypool_available_members",
		Help: "Number of proxy pool members currently usable",
	})

	// FallbackDirectTotal 全池熔断后回退直连的次数。
	FallbackDirectTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "proxypool_fallback_direct_total",
		Help: "Total times the pool was exhausted and traffic fell back to direct",
	})
)

func observe(m *Member, ok bool) {
	result := "fail"
	if ok {
		result = "ok"
	}
	MemberRequestTotal.WithLabelValues(m.name, result).Inc()
}

func (p *Pool) refreshGauges() {
	if p == nil {
		return
	}
	avail := 0
	for _, m := range p.members {
		if m.Healthy() {
			avail++
			MemberHealthy.WithLabelValues(m.name).Set(1)
		} else {
			MemberHealthy.WithLabelValues(m.name).Set(0)
		}
	}
	AvailableMembers.Set(float64(avail))
}
