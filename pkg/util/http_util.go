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
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"dingospeed/pkg/common"
	"dingospeed/pkg/config"
	"dingospeed/pkg/consts"
	"dingospeed/pkg/prom"

	"github.com/avast/retry-go"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

var ProxyIsAvailable = true

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

// buildURL 构建请求URL，根据代理可用性选择主URL或备用URL
func buildURL(requestURL string) (string, error) {
	// 解析输入的请求URL
	parsedURL, err := url.Parse(requestURL)
	if err != nil {
		return "", fmt.Errorf("解析请求URL失败: %w", err)
	}

	// 根据代理配置确定基础URL（baseURL）
	var baseURL string
	if ProxyIsAvailable || !config.SysConfig.DynamicProxy.Enabled {
		baseURL = config.SysConfig.GetHFURLBase()
	} else {
		baseURL = config.SysConfig.GetBpHFURLBase()
	}

	// 构建目标URL（包含路径、查询参数和锚点）
	targetURL := baseURL + parsedURL.Path

	// 追加查询参数（若存在）
	if parsedURL.RawQuery != "" {
		targetURL += "?" + parsedURL.RawQuery
	}

	// 追加锚点（若存在）
	if parsedURL.Fragment != "" {
		targetURL += "#" + parsedURL.Fragment
	}

	return targetURL, nil
}

// NewHTTPClientWithProxy 根据代理地址、超时时间和代理可用性创建HTTP客户端
func NewHTTPClientWithProxy(proxyAddr string, timeout time.Duration, proxyIsAvailable bool) (*http.Client, error) {
	client := &http.Client{Timeout: timeout}
	if !proxyIsAvailable || proxyAddr == "" {
		if proxyAddr == "" {
			zap.S().Warnf("未配置代理，使用默认HTTP客户端")
		}
		return client, nil
	}

	proxyURL, err := url.Parse(proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("代理地址解析失败: %v", err)
	}

	transport := &http.Transport{
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

	client.Transport = transport
	zap.S().Warnf("已启用HTTP代理: %s", proxyAddr)
	return client, nil
}

// Head 方法用于发送带请求头的 HEAD 请求
func Head(requestURL string, headers map[string]string, timeout time.Duration) (*common.Response, error) {
	targetURL, err := buildURL(requestURL)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("HEAD", targetURL, nil)
	if err != nil {
		return nil, fmt.Errorf("创建HEAD请求失败: %v", err)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}

	client, clientErr := NewHTTPClientWithProxy(config.SysConfig.GetHttpProxy(), timeout, ProxyIsAvailable)
	if clientErr != nil {
		return nil, clientErr
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
		respHeaders[key] = values
	}
	return &common.Response{
		StatusCode: resp.StatusCode,
		Headers:    respHeaders,
	}, nil
}

// Get 方法用于发送带请求头的 GET 请求
func Get(requestURL string, headers map[string]string, timeout time.Duration) (*common.Response, error) {
	targetURL, err := buildURL(requestURL)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("GET", targetURL, nil)
	if err != nil {
		return nil, fmt.Errorf("创建GET请求失败: %v", err)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}

	client, clientErr := NewHTTPClientWithProxy(config.SysConfig.GetHttpProxy(), timeout, ProxyIsAvailable)
	if clientErr != nil {
		return nil, clientErr
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
		respHeaders[key] = values
	}

	return &common.Response{
		StatusCode: resp.StatusCode,
		Headers:    respHeaders,
		Body:       body,
	}, nil
}

// GetStream 方法用于发送带请求头的 GET 请求并流式处理响应
func GetStream(requestURL string, headers map[string]string, timeout time.Duration, f func(r *http.Response) error) error {
	targetURL, err := buildURL(requestURL)
	if err != nil {
		return err
	}

	escapedURL := strings.ReplaceAll(targetURL, "#", "%23")
	req, err := http.NewRequest("GET", escapedURL, nil)
	if err != nil {
		return fmt.Errorf("创建GET请求失败: %v", err)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}

	client, clientErr := NewHTTPClientWithProxy(config.SysConfig.GetHttpProxy(), timeout, ProxyIsAvailable)
	if clientErr != nil {
		return clientErr
	}

	resp, err := client.Do(req)
	if err != nil {
		zap.S().Warnf("URL请求失败: %s, 错误: %v", escapedURL, err)
		return fmt.Errorf("执行GET请求失败: %v", err)
	}

	defer resp.Body.Close()
	respHeaders := make(map[string]interface{})
	for key, value := range resp.Header {
		respHeaders[key] = value
	}
	return f(resp)
}

// Post 方法用于发送带请求头的 POST 请求
func Post(requestURL string, contentType string, data []byte, headers map[string]string) (*common.Response, error) {
	targetURL, err := buildURL(requestURL)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", targetURL, bytes.NewBuffer(data))
	if err != nil {
		return nil, fmt.Errorf("创建POST请求失败: %v", err)
	}

	req.Header.Set("Content-Type", contentType)
	for key, value := range headers {
		req.Header.Set(key, value)
	}

	client, clientErr := NewHTTPClientWithProxy(config.SysConfig.GetHttpProxy(), 0, ProxyIsAvailable)
	if clientErr != nil {
		return nil, clientErr
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
		respHeaders[key] = values
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
		c.Response().Header().Set(k, v)
	}
	c.Response().WriteHeader(http.StatusOK)
	flusher, ok := c.Response().Writer.(http.Flusher)
	if !ok {
		return c.String(http.StatusInternalServerError, "Streaming unsupported!")
	}
	for {
		select {
		case b, ok := <-content:
			if !ok {
				zap.S().Infof("ResponseStream complete, %s.", fileName)
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
					prom.PromRequestByteCounter(prom.RequestResponseByte, source, int64(len(b)), orgRepo)
				}
			}
			flusher.Flush()
		}
	}
}

func GetDomain(hfURL string) (string, error) {
	parsedURL, err := url.Parse(hfURL)
	if err != nil {
		return "", err
	}
	return parsedURL.Host, nil
}

// ForwardRequest 用于转发HTTP请求到目标URL，返回统一的Response结构
func ForwardRequest(requestURL string, originalReq *http.Request, timeout time.Duration) (*common.Response, error) {
	// 构建完整的目标URL
	parsedURL, err := url.Parse(requestURL)
	if err != nil {
		return nil, fmt.Errorf("解析目标URL失败: %v", err)
	}

	// 组合目标路径和原始请求路径
	fullPath := parsedURL.String() + originalReq.URL.Path
	if originalReq.URL.RawQuery != "" {
		fullPath += "?" + originalReq.URL.RawQuery
	}

	targetURL, err := buildURL(fullPath)
	if err != nil {
		return nil, err
	}

	// 创建转发请求
	proxyReq, err := http.NewRequest(originalReq.Method, targetURL, originalReq.Body)
	if err != nil {
		return nil, fmt.Errorf("创建转发请求失败: %v", err)
	}

	// 复制原始请求头
	for key, values := range originalReq.Header {
		for _, value := range values {
			proxyReq.Header.Add(key, value)
		}
	}

	// 创建带代理和超时的HTTP客户端
	client, clientErr := NewHTTPClientWithProxy(config.SysConfig.GetHttpProxy(), timeout, ProxyIsAvailable)
	if clientErr != nil {
		return nil, clientErr
	}

	// 发送转发请求
	resp, err := client.Do(proxyReq)
	if err != nil {
		zap.S().Warnf("转发请求失败: %s, 错误: %v", targetURL, err)
		return nil, fmt.Errorf("执行转发请求失败: %v", err)
	}

	// 确保响应体被关闭
	defer func() {
		if r := recover(); r != nil {
			zap.S().Errorf("关闭响应体资源时出现异常: %v", r)
		}
		resp.Body.Close()
	}()

	// 读取响应体内容
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取响应体失败: %v", err)
	}

	// 处理响应头
	respHeaders := make(map[string]interface{})
	for key, values := range resp.Header {
		respHeaders[key] = values
	}

	// 返回统一的响应结构
	return &common.Response{
		StatusCode: resp.StatusCode,
		Headers:    respHeaders,
		Body:       body,
	}, nil
}
