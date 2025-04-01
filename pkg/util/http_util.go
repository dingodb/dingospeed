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
	"io"
	"net/http"
	"net/url"
	"time"

	"dingo-hfmirror/pkg/common"
	"dingo-hfmirror/pkg/consts"

	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

// Head 方法用于发送带请求头的 HEAD 请求
func Head(url string, headers map[string]string, timeout time.Duration) (*common.Response, error) {
	req, err := http.NewRequest("HEAD", url, nil)
	if err != nil {
		return nil, err
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	client := &http.Client{}
	client.Timeout = timeout
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	respHeaders := make(map[string]interface{})
	for key, value := range resp.Header {
		respHeaders[key] = value
	}
	return &common.Response{
		StatusCode: resp.StatusCode,
		Headers:    respHeaders,
	}, nil
}

// Get 方法用于发送带请求头的 GET 请求
func Get(url string, headers map[string]string, timeout time.Duration) (*common.Response, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	client := &http.Client{}
	client.Timeout = timeout
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	respHeaders := make(map[string]interface{})
	for key, value := range resp.Header {
		respHeaders[key] = value
	}
	return &common.Response{
		StatusCode: resp.StatusCode,
		Headers:    respHeaders,
		Body:       body,
	}, nil
}

func GetStream(url string, headers map[string]string, timeout time.Duration) (*common.Response, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	client := &http.Client{}
	client.Timeout = timeout
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	respHeaders := make(map[string]interface{})
	for key, value := range resp.Header {
		respHeaders[key] = value
	}
	var bodyStreamChan = make(chan []byte, consts.RespChanSize)
	go func() {
		defer resp.Body.Close()
		defer close(bodyStreamChan)
		buffer := make([]byte, 1024*1024) // 定义缓冲区大小
		for {
			n, err := resp.Body.Read(buffer)
			if n > 0 {
				bodyStreamChan <- buffer[:n]
			}
			if err != nil {
				if err == io.EOF {
					break
				}
				zap.S().Errorf("读取响应出错: %v\n", err)
				break
			}
		}
	}()
	return &common.Response{
		StatusCode: resp.StatusCode,
		Headers:    respHeaders,
		BodyChan:   bodyStreamChan,
	}, nil
}

// Post 方法用于发送带请求头的 POST 请求
func Post(url string, contentType string, data []byte, headers map[string]string) (*common.Response, error) {
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	respHeaders := make(map[string]interface{})
	for key, value := range resp.Header {
		respHeaders[key] = value
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
				zap.S().Debugf("ResponseStream complete, %s.", fileName)
				return nil
			}
			// bytes.Buffer
			// reader := bytes.NewReader(b)
			// if err := c.Stream(http.StatusOK, "text/event-stream", reader); err != nil {
			// 	zap.S().Errorf("ResponseStream write err,file:%s,%v", fileName, err)
			// 	return nil, ErrorProxyTimeout(c)
			// }

			// test byte
			// data := fmt.Sprintf("Message %d", i)
			// // data := make([]byte, 2048)
			// result = append(result, b...)
			// fmt.Printf("no:%v,result:%d\n", data, len(result))

			// if _, err := c.Response().Write([]byte(data)); err != nil {
			// 	zap.S().Errorf("ResponseStream write err,file:%s,%v", fileName, err)
			// 	time.Sleep(10 * time.Minute)
			// 	return nil, ErrorProxyTimeout(c)
			// }
			// origin
			if _, err := c.Response().Write(b); err != nil {
				zap.S().Errorf("ResponseStream write err,file:%s,%v", fileName, err)
				return ErrorProxyTimeout(c)
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
