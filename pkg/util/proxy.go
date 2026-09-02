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

package util

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"dingospeed/pkg/config"
	"dingospeed/pkg/proxypool"
)

var globalPool atomic.Pointer[proxypool.Pool]

// InitProxyPool 依据配置构建全局代理池并启动探活。进程启动时调用一次。
// 配置有误时返回 error 让进程启动失败：代理池是回源的唯一通路，
// 带着一个「看起来配了、实际没生效」的池子跑起来比起不来更难排查。
func InitProxyPool() error {
	cfg, ok := buildPoolConfig()
	if !ok {
		zap.S().Info("代理池未启用，回源走直连或旧的单代理逻辑")
		return nil
	}
	p, err := proxypool.New(cfg)
	if err != nil {
		return fmt.Errorf("代理池初始化失败: %w", err)
	}
	if p == nil {
		return nil
	}
	globalPool.Store(p)
	p.StartProbe()
	names := make([]string, 0, p.Size())
	for _, m := range p.Members() {
		names = append(names, m.Name())
	}
	zap.S().Infof("代理池已启用，成员 %d 个: %v", p.Size(), names)
	return nil
}

// ProxyPool 返回全局代理池，未启用时返回 nil。
func ProxyPool() *proxypool.Pool {
	return globalPool.Load()
}

var (
	fallbackLogMu   sync.Mutex
	fallbackLogLast time.Time
)

// logFallbackThrottled 全池熔断的告警日志每分钟最多一条，
// 精确次数由 proxypool_fallback_direct_total 指标承载。
func logFallbackThrottled() {
	fallbackLogMu.Lock()
	defer fallbackLogMu.Unlock()
	if time.Since(fallbackLogLast) < time.Minute {
		return
	}
	fallbackLogLast = time.Now()
	zap.S().Warnf("代理池全部熔断，请求回退直连 %s（本条日志每分钟最多一次）",
		config.SysConfig.GetBpHFURLBase())
}

func buildPoolConfig() (proxypool.Config, bool) {
	pc := config.SysConfig.ProxyPool
	members := make([]proxypool.MemberConfig, 0, len(pc.Members))
	for _, m := range pc.Members {
		members = append(members, proxypool.MemberConfig{Name: m.Name, URL: m.URL, Weight: m.Weight})
	}
	if !pc.Enabled || len(members) == 0 {
		// 兼容旧配置：单个 httpProxy 退化成单成员池。
		if config.SysConfig.GetHttpProxy() == "" {
			return proxypool.Config{}, false
		}
		name := config.SysConfig.GetHttpProxyName()
		if name == "" {
			name = "legacy"
		}
		members = []proxypool.MemberConfig{{Name: name, URL: config.SysConfig.GetHttpProxy(), Weight: 1}}
	}

	failThreshold := pc.FailThreshold
	if failThreshold <= 0 {
		// 旧配置里的 maxContinuousFails 语义一致，直接继承。
		failThreshold = config.SysConfig.GetMaxContinuousFails()
	}

	return proxypool.Config{
		Enabled:       true,
		Members:       members,
		ProbeTarget:   pc.ProbeTarget,
		ProbeInterval: secOr(pc.ProbeInterval, config.SysConfig.GetDynamicProxyTimePeriod()),
		ProbeTimeout:  secOr(pc.ProbeTimeout, 10*time.Second),
		FailThreshold: failThreshold,
		Cooldown:      secOr(pc.Cooldown, 5*time.Minute),
		DialTimeout:   secOr(pc.DialTimeout, 10*time.Second),
		// 沿用既有 download.reqTimeout 语义（默认 0 = 不限），
		// 大文件流式下载靠它保持不被整体超时硬砍。
		ReqTimeout:            config.SysConfig.GetReqTimeOut(),
		ResponseHeaderTimeout: secOr(pc.ResponseHeaderTimeout, 60*time.Second),
		NoProxy:               pc.NoProxy,
	}, true
}

func secOr(seconds int, fallback time.Duration) time.Duration {
	if seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if fallback > 0 {
		return fallback
	}
	return 0
}
