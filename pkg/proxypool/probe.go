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
	"context"
	"io"
	"net/http"
	"sync"
	"time"
)

// defaultProbeTarget 用真实回源域名做探活。
// 旧实现探 www.google.com —— 国内本就不通，探活必然失败，等于没探。
// 探活目标必须是“这个代理实际要去的地方”。
const defaultProbeTarget = "https://hf-mirror.com/api/models/bert-base-uncased"

// StartProbe 启动后台探活循环，直到 Close 被调用。
// 探活只做两件事：把恢复了的成员尽早放回轮转、把已经坏掉但还没被流量打中的成员提前摘掉。
func (p *Pool) StartProbe() {
	if p == nil || len(p.members) == 0 {
		return
	}
	target := p.cfg.ProbeTarget
	if target == "" {
		target = defaultProbeTarget
	}
	go func() {
		ticker := time.NewTicker(p.cfg.ProbeInterval)
		defer ticker.Stop()
		p.probeAll(target)
		for {
			select {
			case <-p.stopCh:
				return
			case <-ticker.C:
				p.probeAll(target)
			}
		}
	}()
}

func (p *Pool) probeAll(target string) {
	var wg sync.WaitGroup
	for _, m := range p.members {
		wg.Add(1)
		go func(m *Member) {
			defer wg.Done()
			code, err := probeOnce(m, target, p.cfg.ProbeTimeout)
			// 探活结果与业务流量走同一套计分：探通即清零熔断，探不通累计失败。
			p.Report(m, code, err)
		}(m)
	}
	wg.Wait()
	// 健康度 gauge 统一在这里刷新，避免在下载热路径的 Report 里遍历全部成员。
	p.refreshGauges()
}

func probeOnce(m *Member, target string, timeout time.Duration) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", "dingospeed-proxypool/1.0")
	// 探活单独用短超时的客户端，不能复用成员那个 Timeout=0 的下载客户端。
	client := &http.Client{Timeout: timeout, Transport: m.client.Transport}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	// 必须真读一段 body：CONNECT 建连成功、响应头也回来了、
	// 但隧道内传输被掐断的情况只有读 body 才暴露得出来。
	_, err = io.CopyN(io.Discard, resp.Body, 1024)
	if err != nil && err != io.EOF {
		return resp.StatusCode, err
	}
	return resp.StatusCode, nil
}
