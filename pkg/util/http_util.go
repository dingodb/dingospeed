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
)

// Get 方法用于发送带请求头的 GET 请求
func Get(url string, headers map[string]string, timeout time.Duration) ([]byte, error) {
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
	return body, nil
}

// Head 方法用于发送带请求头的 HEAD 请求
func Head(url string, headers map[string]string, timeout time.Duration) (*http.Response, error) {
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
	return resp, nil
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

func GetDomain(hfURL string) (string, error) {
	parsedURL, err := url.Parse(hfURL)
	if err != nil {
		return "", err
	}
	return parsedURL.Host, nil
}
