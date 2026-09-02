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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"dingospeed/pkg/common"
	"dingospeed/pkg/config"
	"dingospeed/pkg/consts"
	"dingospeed/pkg/prom"
	"dingospeed/pkg/proxypool"

	"github.com/avast/retry-go"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

var (
	ProxyIsAvailable = true
	simpleClient     *http.Client
	proxyClient      *http.Client
	simpleOnce       sync.Once
	proxyOnce        sync.Once
)

func RetryRequest(f func() (*common.Response, error)) (*common.Response, error) {
	var resp *common.Response
	err := retry.Do(
		func() error {
			var err error
			resp, err = f()
			return err
		},
		retry.Delay(time.Duration(config.SysConfig.Retry.Delay)*time.Second),
		retry.Attempts(config.SysConfig.Retry.Attempts),
		retry.DelayType(retry.FixedDelay),
	)
	return resp, err
}

func NewHTTPClient(method string) (*http.Client, error) {
	if method == http.MethodHead {
		return &http.Client{
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse // 阻止跟随重定向
			},
			Timeout: config.SysConfig.GetReqTimeOut()}, nil
	}
	simpleOnce.Do(
		func() {
			simpleClient = &http.Client{Timeout: config.SysConfig.GetReqTimeOut()}
		})
	return simpleClient, nil
}

func NewHTTPClientWithProxy(method string) (*http.Client, error) {
	var transport *http.Transport
	if config.SysConfig.GetHttpProxy() != "" {
		proxyURL, err := url.Parse(config.SysConfig.GetHttpProxy())
		if err != nil {
			zap.S().Errorf("代理地址解析失败: %v", err)
			return nil, err
		}
		transport = &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ForceAttemptHTTP2:     false,
			ResponseHeaderTimeout: 10 * time.Second,
			IdleConnTimeout:       90 * time.Second,
		}
	}
	if method == http.MethodHead {
		proxyHeadClient := &http.Client{
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse // 阻止跟随重定向
			},
			Timeout: config.SysConfig.GetReqTimeOut()}
		if transport != nil {
			proxyHeadClient.Transport = transport
		}
		return proxyHeadClient, nil
	}
	proxyOnce.Do(func() {
		proxyClient = &http.Client{Timeout: config.SysConfig.GetReqTimeOut()}
		if transport != nil {
			proxyClient.Transport = transport
		}
	})
	return proxyClient, nil
}

func constructClient(method string) (string, *http.Client, error) {
	var (
		domain string
		client *http.Client
		err    error
	)
	// 代理不可用，且允许代理切换到备用，使用直联。
	if !ProxyIsAvailable && config.SysConfig.DynamicProxy.Enabled {
		domain = config.SysConfig.GetBpHFURLBase()
		client, err = NewHTTPClient(method)
	} else {
		domain = config.SysConfig.GetHFURLBase()
		client, err = NewHTTPClientWithProxy(method)
	}
	return domain, client, err
}

// route 一次出站请求的选路结果。member 为 nil 表示这条请求没走池子（直连或旧逻辑）。
type route struct {
	domain string
	client *http.Client
	member *proxypool.Member
}

// report 把本次请求结果回灌给代理池，驱动熔断与恢复。
func (r route) report(statusCode int, err error) {
	if r.member != nil {
		ProxyPool().Report(r.member, statusCode, err)
	}
}

func (r route) reportResp(resp *common.Response, err error) {
	code := 0
	if resp != nil {
		code = resp.StatusCode
	}
	r.report(code, err)
}

// constructRoute 为一次出站请求选路。
// 关键语义：每调用一次就从池子里换一个出口，因此 RetryRequest 的三次重试
// 会落在三个不同出口上，而不是把同一条死路撞三遍。
func constructRoute(method string) (route, error) {
	if pool := ProxyPool(); pool != nil {
		if m := pool.Pick(); m != nil {
			return route{
				domain: config.SysConfig.GetHFURLBase(),
				client: m.Client(method),
				member: m,
			}, nil
		}
		// 全池熔断：所有出口都不可用，回退直连备用域名，至少保证有降级路径。
		if config.SysConfig.GetProxyPoolFallbackDirect() {
			proxypool.FallbackDirectTotal.Inc()
			// 故障时 QPS 不会下降，逐请求打日志会在最需要看日志的时候把日志刷爆。
			logFallbackThrottled()
			client, err := NewHTTPClient(method)
			return route{domain: config.SysConfig.GetBpHFURLBase(), client: client}, err
		}
	}
	domain, client, err := constructClient(method)
	return route{domain: domain, client: client}, err
}

func Head(requestUri string, headers map[string]string) (*common.Response, error) {
	r, err := constructRoute(http.MethodHead)
	if err != nil {
		return nil, fmt.Errorf("construct http client err: %v", err)
	}
	requestURL := fmt.Sprintf("%s%s", r.domain, requestUri)
	resp, err := doHead(r.client, requestURL, headers)
	r.reportResp(resp, err)
	return resp, err
}

func doHead(client *http.Client, targetURL string, headers map[string]string) (*common.Response, error) {
	req, err := http.NewRequest("HEAD", targetURL, nil)
	if err != nil {
		return nil, fmt.Errorf("创建HEAD请求失败: %v", err)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := client.Do(req)
	if err != nil {
		zap.S().Warnf("URL请求失败: %s, 错误: %v", targetURL, err)
		return nil, fmt.Errorf("执行HEAD请求失败: %v", err)
	}
	defer func() {
		if r := recover(); r != nil {
			zap.S().Errorf("关闭响应体资源时出现异常: %v", r)
		}
		resp.Body.Close()
	}()
	respHeaders := make(map[string]interface{})
	for key, values := range resp.Header {
		respHeaders[strings.ToLower(key)] = values
	}
	return &common.Response{
		StatusCode: resp.StatusCode,
		Headers:    respHeaders,
	}, nil
}

func Get(requestUri string, headers map[string]string) (*common.Response, error) {
	r, err := constructRoute(http.MethodGet)
	if err != nil {
		return nil, fmt.Errorf("construct http client err: %v", err)
	}
	requestURL := fmt.Sprintf("%s%s", r.domain, requestUri)
	resp, err := doGet(r.client, requestURL, headers)
	r.reportResp(resp, err)
	return resp, err
}

func doGet(client *http.Client, targetURL string, headers map[string]string) (*common.Response, error) {
	req, err := http.NewRequest("GET", targetURL, nil)
	if err != nil {
		return nil, fmt.Errorf("创建GET请求失败: %v", err)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := client.Do(req)
	if err != nil {
		zap.S().Warnf("URL请求失败: %s, 错误: %v", targetURL, err)
		return nil, fmt.Errorf("执行GET请求失败: %v", err)
	}

	defer func() {
		if r := recover(); r != nil {
			zap.S().Errorf("关闭响应体资源时出现异常: %v", r)
		}
		resp.Body.Close()
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取响应体失败: %v", err)
	}

	respHeaders := make(map[string]interface{})
	for key, values := range resp.Header {
		respHeaders[strings.ToLower(key)] = values
	}

	return &common.Response{
		StatusCode: resp.StatusCode,
		Headers:    respHeaders,
		Body:       body,
	}, nil
}

func GetStream(domain, uri string, headers map[string]string, f func(r *http.Response) error) error {
	// 内网目标（兄弟节点互拉、回环上传口）必须旁路代理：
	// 这些地址走公网出口必然失败，且会把好出口误判成坏出口。
	if IsInnerDomain(domain) || ProxyPool().ShouldBypass(domain) {
		client, err := NewHTTPClient(http.MethodGet)
		if err != nil {
			return fmt.Errorf("construct http client err: %v", err)
		}
		headers[consts.RequestSourceInner] = Itoa(1)
		_, err = doGetStream(client, fmt.Sprintf("%s%s", domain, uri), headers, f)
		return err
	}
	r, err := constructRoute(http.MethodGet)
	if err != nil {
		return fmt.Errorf("construct http client err: %v", err)
	}
	requestURL := fmt.Sprintf("%s%s", r.domain, uri)
	code, err := doGetStream(r.client, requestURL, headers, f)
	// 这里上报的 err 包含了流式传输中途的失败（f 回调里读 body 断掉），
	// 而这正是 gost 的 TCP 层健康检查看不见的那一类故障。
	// 但客户端主动取消不能算在出口头上，否则用户中断下载会误伤健康出口。
	if !isClientCanceled(err) {
		r.report(code, err)
	}
	return err
}

// isClientCanceled 判断错误是否源自本地取消（用户中断下载、下游关闭），
// 这类错误与出口健康无关。
func isClientCanceled(err error) bool {
	return err != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded))
}

// doGetStream 返回响应状态码与错误，状态码供代理池计分使用；未拿到响应时返回 0。
func doGetStream(client *http.Client, targetURL string, headers map[string]string, f func(r *http.Response) error) (int, error) {
	escapedURL := strings.ReplaceAll(targetURL, "#", "%23")
	req, err := http.NewRequest("GET", escapedURL, nil)
	if err != nil {
		return 0, fmt.Errorf("创建GET请求失败: %v", err)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	respHeaders := make(map[string]interface{})
	for key, value := range resp.Header {
		respHeaders[strings.ToLower(key)] = value
	}
	return resp.StatusCode, f(resp)
}

func Post(requestUri string, contentType string, data []byte, headers map[string]string) (*common.Response, error) {
	r, err := constructRoute(http.MethodPost)
	if err != nil {
		return nil, fmt.Errorf("construct http client err: %v", err)
	}
	requestURL := fmt.Sprintf("%s%s", r.domain, requestUri)
	resp, err := doPost(r.client, requestURL, contentType, data, headers)
	r.reportResp(resp, err)
	return resp, err
}

func doPost(client *http.Client, targetURL string, contentType string, data []byte, headers map[string]string) (*common.Response, error) {
	req, err := http.NewRequest("POST", targetURL, bytes.NewBuffer(data))
	if err != nil {
		return nil, fmt.Errorf("创建POST请求失败: %v", err)
	}

	req.Header.Set("Content-Type", contentType)
	for key, value := range headers {
		req.Header.Set(key, value)
	}

	resp, err := client.Do(req)
	if err != nil {
		zap.S().Warnf("URL请求失败: %s, 错误: %v", targetURL, err)
		return nil, fmt.Errorf("执行POST请求失败: %v", err)
	}

	defer func() {
		if r := recover(); r != nil {
			zap.S().Errorf("关闭响应体资源时出现异常: %v", r)
		}
		resp.Body.Close()
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取响应体失败: %v", err)
	}

	respHeaders := make(map[string]interface{})
	for key, values := range resp.Header {
		respHeaders[strings.ToLower(key)] = values
	}

	return &common.Response{
		StatusCode: resp.StatusCode,
		Headers:    respHeaders,
		Body:       body,
	}, nil
}

func ResponseStream(c echo.Context, fileName string, headers map[string]string, content <-chan []byte) error {
	c.Response().Header().Set("Content-Type", "text/event-stream")
	c.Response().Header().Set("Cache-Control", "no-cache")
	c.Response().Header().Set("Connection", "keep-alive")
	for k, v := range headers {
		// 流式响应不应预设Content-Length，因为数据是流式传输的，无法保证字节数与预估值一致。
		// 若实际传输字节数与Content-Length不符，客户端会报错（如curl: (18) transfer closed with N bytes remaining to read）。
		c.Response().Header().Set(k, v)
	}
	// 根据 headers 中是否包含 Content-Range 来决定状态码
	statusCode := http.StatusOK
	if c.Response().Header().Get("Content-Range") != "" {
		statusCode = http.StatusPartialContent
	}
	c.Response().WriteHeader(statusCode)
	flusher, ok := c.Response().Writer.(http.Flusher)
	if !ok {
		return c.String(http.StatusInternalServerError, "Streaming unsupported!")
	}
	for {
		select {
		case b, ok := <-content:
			if !ok {
				zap.S().Infof("ResponseStream complete, %s", fileName)
				return nil
			}
			if len(b) > 0 {
				if _, err := c.Response().Write(b); err != nil {
					zap.S().Warnf("ResponseStream write err,file:%s,%v", fileName, err)
					return ErrorProxyTimeout(c)
				}
				if config.SysConfig.EnableMetric() {
					// 原子性地更新响应总数
					source := Itoa(c.Get(consts.PromSource))
					orgRepo := Itoa(c.Get(consts.PromOrgRepo))
					prom.PromResponseByteCounter(prom.RequestResponseByte, source, orgRepo, int64(len(b)))
				}
			}
			flusher.Flush()
		}
	}
}

func ForwardRequest(originalReq echo.Context) (*http.Response, error) {
	r, err := constructRoute(http.MethodGet)
	if err != nil {
		return nil, fmt.Errorf("construct http client err: %v", err)
	}
	client := r.client
	reqUri := originalReq.Request().URL.Path
	targetURL, err := url.Parse(r.domain)
	if err != nil {
		return nil, fmt.Errorf("url.Parse err: %v", err)
	}
	forwardPath := targetURL.Path + reqUri
	forwardURL := &url.URL{
		Scheme:   targetURL.Scheme,
		Host:     targetURL.Host,
		Path:     forwardPath,
		RawQuery: originalReq.Request().URL.RawQuery,
	}
	proxyReq, err := http.NewRequest(originalReq.Request().Method, forwardURL.String(), originalReq.Request().Body)
	if err != nil {
		return nil, fmt.Errorf("创建转发请求失败: %v", err)
	}
	for key, values := range originalReq.Request().Header {
		for _, value := range values {
			proxyReq.Header.Add(key, value)
		}
	}
	resp, err := client.Do(proxyReq)
	if err != nil {
		r.report(0, err)
		zap.S().Warnf("转发请求失败: %s, 错误: %v", targetURL, err)
		return nil, fmt.Errorf("执行转发请求失败: %v", err)
	}
	r.report(resp.StatusCode, nil)
	return resp, nil
}

func IsInnerDomain(url string) bool {
	return !strings.Contains(url, consts.Huggingface) && !strings.Contains(url, consts.Hfmirror)
}
