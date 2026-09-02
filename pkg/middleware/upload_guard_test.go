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

package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
)

func runGuard(t *testing.T, method, path string, headers map[string]string) (int, bool) {
	t.Helper()
	e := echo.New()
	reached := false
	h := UploadGuardMiddleware()(func(c echo.Context) error {
		reached = true
		return c.NoContent(http.StatusOK)
	})
	req := httptest.NewRequest(method, path, strings.NewReader(`{"items":[]}`))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	if err := h(e.NewContext(req, rec)); err != nil {
		t.Fatalf("middleware err: %v", err)
	}
	return rec.Code, reached
}

// 核心回归：恶意网页用 text/plain 的 POST 发起简单请求（不触发预检），
// 试图删掉缓存。浏览器一定会带上 Origin，据此拦截。
func TestGuardBlocksCrossOriginDelete(t *testing.T) {
	code, reached := runGuard(t, http.MethodPost, "/api/cache/orphans/delete", map[string]string{
		"Origin":       "https://evil.example.com",
		"Content-Type": "text/plain",
	})
	if reached {
		t.Fatal("跨站写请求不应到达 handler")
	}
	if code != http.StatusForbidden {
		t.Fatalf("应返回 403，实际 %d", code)
	}
}

// 没有 Origin 但 Sec-Fetch-Site 表明是跨站，同样拦掉。
func TestGuardBlocksBySecFetchSite(t *testing.T) {
	for _, site := range []string{"cross-site", "same-site"} {
		code, reached := runGuard(t, http.MethodPost, "/api/local-publish/models/a/b/main",
			map[string]string{"Sec-Fetch-Site": site})
		if reached || code != http.StatusForbidden {
			t.Fatalf("Sec-Fetch-Site=%s 应被拦截，code=%d reached=%v", site, code, reached)
		}
	}
}

// ingest agent、curl 这类服务端调用方不带 Origin，必须放行。
func TestGuardAllowsServerToServer(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		code, reached := runGuard(t, method, "/api/cache/files/delete", map[string]string{
			"Content-Type": "application/json",
		})
		if !reached {
			t.Fatalf("%s 无 Origin 的服务端调用应放行，code=%d", method, code)
		}
	}
}

// 用户在地址栏直接访问（Sec-Fetch-Site: none）不算跨站。
func TestGuardAllowsSameOriginAndNone(t *testing.T) {
	for _, site := range []string{"none", "same-origin"} {
		if _, reached := runGuard(t, http.MethodPost, "/api/cache/files/delete",
			map[string]string{"Sec-Fetch-Site": site}); !reached {
			t.Fatalf("Sec-Fetch-Site=%s 应放行", site)
		}
	}
}

// 只读方法不受影响：跨站也读不到响应，因为上传引擎不再发 CORS 头。
func TestGuardIgnoresReadMethods(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		if _, reached := runGuard(t, method, "/api/cache/repos",
			map[string]string{"Origin": "https://evil.example.com"}); !reached {
			t.Fatalf("%s 不应被拦截", method)
		}
	}
}
