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
	"sync"
	"time"

	"dingospeed/pkg/common"
	"dingospeed/pkg/config"
	"dingospeed/pkg/consts"
	"dingospeed/pkg/prom"

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

func NewHTTPClient() (*http.Client, error) {
	simpleOnce.Do(
		func() {
			simpleClient = &http.Client{Timeout: config.SysConfig.GetReqTimeOut()}
		})
	return simpleClient, nil
}

func NewHTTPClientWithProxy() (*http.Client, error) {
	proxyOnce.Do(func() {
		proxyClient = &http.Client{Timeout: config.SysConfig.GetReqTimeOut()}
		if config.SysConfig.GetHttpProxy() == "" {
			return
		}
		proxyURL, err := url.Parse(config.SysConfig.GetHttpProxy())
		if err != nil {
			zap.S().Errorf("代理地址解析失败: %v", err)
			return
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
		proxyClient.Transport = transport
	})
	return proxyClient, nil
}

func constructClient() (string, *http.Client, error) {
	var (
		domain string
		client *http.Client
		err    error
	)
	// 代理不可用，且允许代理切换到备用，使用直联。
	if !ProxyIsAvailable && config.SysConfig.DynamicProxy.Enabled {
		domain = config.SysConfig.GetBpHFURLBase()
		client, err = NewHTTPClient()
	} else {
		domain = config.SysConfig.GetHFURLBase()
		client, err = NewHTTPClientWithProxy()
	}
	return domain, client, err
}

func Head(requestUri string, headers map[string]string) (*common.Response, error) {
	domain, client, err := constructClient()
	if err != nil {
		return nil, fmt.Errorf("construct http client err: %v", err)
	}
	requestURL := fmt.Sprintf("%s%s", domain, requestUri)
	return doHead(client, requestURL, headers)
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
		respHeaders[key] = values
	}
	return &common.Response{
		StatusCode: resp.StatusCode,
		Headers:    respHeaders,
	}, nil
}

func Get(requestUri string, headers map[string]string) (*common.Response, error) {
	domain, client, err := constructClient()
	if err != nil {
		return nil, fmt.Errorf("construct http client err: %v", err)
	}
	requestURL := fmt.Sprintf("%s%s", domain, requestUri)
	return doGet(client, requestURL, headers)
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
		respHeaders[key] = values
	}

	return &common.Response{
		StatusCode: resp.StatusCode,
		Headers:    respHeaders,
		Body:       body,
	}, nil
}

func GetStream(domain, uri string, headers map[string]string, f func(r *http.Response) error) error {
	var (
		client *http.Client
		err    error
	)
	if IsInnerDomain(domain) {
		client, err = NewHTTPClient()
		headers[consts.RequestSourceInner] = Itoa(1)
	} else {
		domain, client, err = constructClient()
	}
	if err != nil {
		return fmt.Errorf("construct http client err: %v", err)
	}
	requestURL := fmt.Sprintf("%s%s", domain, uri)
	return doGetStream(client, requestURL, headers, f)
}

func doGetStream(client *http.Client, targetURL string, headers map[string]string, f func(r *http.Response) error) error {
	escapedURL := strings.ReplaceAll(targetURL, "#", "%23")
	req, err := http.NewRequest("GET", escapedURL, nil)
	if err != nil {
		return fmt.Errorf("创建GET请求失败: %v", err)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respHeaders := make(map[string]interface{})
	for key, value := range resp.Header {
		respHeaders[key] = value
	}
	return f(resp)
}

func Post(requestUri string, contentType string, data []byte, headers map[string]string) (*common.Response, error) {
	domain, client, err := constructClient()
	if err != nil {
		return nil, fmt.Errorf("construct http client err: %v", err)
	}
	requestURL := fmt.Sprintf("%s%s", domain, requestUri)
	return doPost(client, requestURL, contentType, data, headers)
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
	domain, client, err := constructClient()
	if err != nil {
		return nil, fmt.Errorf("construct http client err: %v", err)
	}
	targetURL, err := url.Parse(domain)
	if err != nil {
		return nil, fmt.Errorf("url.Parse err: %v", err)
	}
	forwardPath := targetURL.Path + originalReq.Request().URL.Path
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
		zap.S().Warnf("转发请求失败: %s, 错误: %v", targetURL, err)
		return nil, fmt.Errorf("执行转发请求失败: %v", err)
	}
	return resp, nil
}

func IsInnerDomain(url string) bool {
	return !strings.Contains(url, consts.Huggingface) && !strings.Contains(url, consts.Hfmirror)
}
