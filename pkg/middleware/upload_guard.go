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

	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

// UploadGuardMiddleware 挡住浏览器发起的跨站请求。
//
// 上传口没有任何身份校验，安全性完全建立在“只有本机的 ingest agent 会调它”
// 这个假设上。但绑在 127.0.0.1 挡不住浏览器——用户的浏览器就在回环上。
//
// 具体的攻击路径：上传口的写接口全部读裸 body、不校验 Content-Type，
// 于是一个 text/plain 的 POST 就能带着 JSON 打进来。而 text/plain 的 POST
// 属于 CORS 简单请求，不触发预检，浏览器会直接送出去。用户访问任意一个恶意
// 页面，那个页面就能 POST /api/cache/orphans/delete 把缓存删掉。
//
// 这里用 Origin 头来区分：浏览器发起的跨站请求一定带 Origin（简单请求也带），
// 而 ingest agent、curl 这类服务端调用方不会带。因此拒掉“带 Origin 的写请求”
// 既能挡住浏览器，又不需要任何调用方改代码。Sec-Fetch-Site 是同一判断的补强，
// 现代浏览器都会发，且是禁止修改的头。
//
// 这不能替代真正的鉴权，只是把浏览器这条路堵死。任何能直接发 HTTP 的进程
// 仍然可以访问上传口——那个问题要靠给上传口加回身份校验来解决。
func UploadGuardMiddleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			req := c.Request()
			if !isStateChanging(req.Method) {
				return next(c)
			}
			if origin := req.Header.Get("Origin"); origin != "" {
				zap.S().Warnf("[UPLOAD] 拒绝跨站写请求: method=%s path=%s origin=%s",
					req.Method, req.URL.Path, origin)
				return c.JSON(http.StatusForbidden, map[string]string{
					"code":  "UPLOAD_CROSS_ORIGIN_DENIED",
					"error": "cross-origin requests are not allowed on the upload port",
				})
			}
			// none = 用户直接输地址；same-origin = 同源页面。其余（cross-site、
			// same-site）都是别的站点发起的。
			if site := req.Header.Get("Sec-Fetch-Site"); site != "" && site != "none" && site != "same-origin" {
				zap.S().Warnf("[UPLOAD] 拒绝跨站写请求: method=%s path=%s sec-fetch-site=%s",
					req.Method, req.URL.Path, site)
				return c.JSON(http.StatusForbidden, map[string]string{
					"code":  "UPLOAD_CROSS_ORIGIN_DENIED",
					"error": "cross-origin requests are not allowed on the upload port",
				})
			}
			return next(c)
		}
	}
}

func isStateChanging(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}
