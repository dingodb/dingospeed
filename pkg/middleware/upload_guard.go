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

// UploadGuardMiddleware 使用 Origin / Sec-Fetch-Site 拒绝浏览器跨站写入。
// 浏览器简单请求也带 Origin，因此这里不依赖 CORS 预检或 Content-Type。
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
