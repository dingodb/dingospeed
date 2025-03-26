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
	"net/http"

	"github.com/labstack/echo/v4"
)

// 错误码说明
// 1-3 位: HTTP 状态码
// 4 位: 组件
// 5-6 位: 组件内部错误码
var (
	CodeBadRequest     = NewAPICode(400, "请求参数错误")
	CodeBadTimeRequest = NewAPICode(401, "请求的时间参数格式错误")
	CodeUnauthorized   = NewAPICode(401, "认证失败")
	CodeForbidden      = NewAPICode(403, "授权失败")
	CodeNotFound       = NewAPICode(404, "资源未找到")

	CodeOperateFailed = NewAPICode(500, "操作失败")
	CodeSaveFailed    = NewAPICode(501, "增加失败")
	CodeUpdatedFailed = NewAPICode(502, "修改失败")
	CodeDeletedFailed = NewAPICode(503, "删除失败")
	CodeFoundFailed   = NewAPICode(504, "查询失败")
	CodeStartFailed   = NewAPICode(505, "启动失败")
	CodeStopFailed    = NewAPICode(506, "停止失败")
)

func ResponseHeaders(ctx echo.Context, headers map[string]string) error {
	fullHeaders(ctx, headers)
	return ctx.JSON(http.StatusOK, nil)
}

// func Response(ctx *gin.Context, apiCode APICode, cause ...error) {
// 	msg := apiCode.Msg
// 	if len(cause) > 0 {
// 		c := cause[0]
// 		var t myerr.Error
// 		if errors.As(c, &t) {
// 			msg = t.Error()
// 		}
// 	}
// 	ctx.JSON(apiCode.HTTPStatus(), gin.H{
// 		"code": apiCode.Code,
// 		"msg":  msg,
// 	})
// }

func ErrorRepoNotFound(ctx echo.Context) error {
	content := map[string]string{
		"error": "Repository not found",
	}
	headers := map[string]string{
		"x-error-code":    "RepoNotFound",
		"x-error-message": "Repository not found",
	}
	return Response(ctx, http.StatusUnauthorized, headers, content)
}

func ErrorPageNotFound(ctx echo.Context) error {
	content := map[string]string{
		"error": "Sorry, we can't find the page you are looking for.",
	}
	headers := map[string]string{
		"x-error-code":    "RepoNotFound",
		"x-error-message": "Sorry, we can't find the page you are looking for.",
	}
	return Response(ctx, http.StatusNotFound, headers, content)
}

func ErrorEntryNotFoundBranch(ctx echo.Context, branch, path string) error {
	headers := map[string]string{
		"x-error-code":    "EntryNotFound",
		"x-error-message": fmt.Sprintf("%s does not exist on %s", branch, path),
	}
	return Response(ctx, http.StatusUnauthorized, headers, nil)
}

func ErrorEntryNotFound(ctx echo.Context) error {
	headers := map[string]string{
		"x-error-code":    "EntryNotFound",
		"x-error-message": "Entry not found",
	}
	return Response(ctx, http.StatusNotFound, headers, nil)
}

func ErrorProxyTimeout(ctx echo.Context) error {
	headers := map[string]string{
		"x-error-code":    "ProxyTimeout",
		"x-error-message": "Proxy Timeout",
	}
	return Response(ctx, http.StatusGatewayTimeout, headers, nil)
}

func ErrorProxyError(ctx echo.Context) error {
	headers := map[string]string{
		"x-error-code":    "Proxy error",
		"x-error-message": "Proxy error",
	}
	return Response(ctx, http.StatusInternalServerError, headers, nil)
}

func Response(ctx echo.Context, httpStatus int, headers map[string]string, data interface{}) error {
	fullHeaders(ctx, headers)
	return ctx.JSON(httpStatus, data)
}

func fullHeaders(c echo.Context, headers map[string]string) {
	for k, v := range headers {
		c.Response().Header().Set(k, v)
	}
}

type APICode struct {
	Code int         `json:"code"`
	Data interface{} `json:"data"`
	Msg  string      `json:"msg"`
}

func NewAPICode(code int, message string) APICode {
	return APICode{
		Code: code,
		Msg:  message,
	}
}
func (a *APICode) HTTPStatus() int {
	v := a.Code
	v /= 1
	return v
}
