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
	"errors"
	"fmt"
	"net/http"

	myerr "dingospeed/pkg/error"

	"github.com/labstack/echo/v4"
)

func ErrorRepoNotFound(ctx echo.Context) error {
	content := map[string]string{
		"error": "Repository not found",
	}
	headers := map[string]string{
		"x-error-code":    "RepoNotFound",
		"x-error-message": "Repository not found",
	}
	return Response(ctx, http.StatusNotFound, headers, content)
}

func ErrorRequestParam(ctx echo.Context) error {
	content := map[string]string{
		"error": "请求参数错误",
	}
	headers := map[string]string{
		"x-error-code":    "Request param error",
		"x-error-message": "Request param error",
	}
	return Response(ctx, http.StatusBadRequest, headers, content)
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

func ErrorEntryUnknown(ctx echo.Context, statusCode int, msg string) error {
	content := map[string]string{
		"error": msg,
	}
	if statusCode == 0 {
		statusCode = http.StatusInternalServerError
	}
	return Response(ctx, statusCode, nil, content)
}

func ErrorEntryNotFound(ctx echo.Context) error {
	headers := map[string]string{
		"x-error-code":    "EntryNotFound",
		"x-error-message": "Entry not found",
	}
	return Response(ctx, http.StatusNotFound, headers, nil)
}

func ErrorRevisionNotFound(ctx echo.Context, revision string) error {
	content := map[string]string{
		"error": fmt.Sprintf("Invalid rev id: %s", revision),
	}
	headers := map[string]string{
		"x-error-code":    "RevisionNotFound",
		"x-error-message": fmt.Sprintf("Invalid rev id: %s", revision),
	}
	return Response(ctx, http.StatusNotFound, headers, content)
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

func ErrorMethodError(ctx echo.Context) error {
	content := map[string]string{
		"error": "request method error",
	}
	headers := map[string]string{
		"x-error-code":    "request method error",
		"x-error-message": "request method error",
	}
	return Response(ctx, http.StatusInternalServerError, headers, content)
}

func ErrorTooManyRequest(ctx echo.Context) error {
	content := map[string]string{
		"error": "Too many requests",
	}
	return Response(ctx, http.StatusTooManyRequests, nil, content)
}

func ResponseHeaders(ctx echo.Context, code int, headers map[string]string) error {
	fullHeaders(ctx, headers)
	return ctx.JSON(code, nil)
}

func Response(ctx echo.Context, httpStatus int, headers map[string]string, data interface{}) error {
	fullHeaders(ctx, headers)
	return ctx.JSON(httpStatus, data)
}

func ResponseData(ctx echo.Context, data interface{}) error {
	return ctx.JSON(http.StatusOK, data)
}

func NormalResponseData(ctx echo.Context, data interface{}) error {
	return ctx.JSON(http.StatusOK, Body{
		Data: data,
		Msg:  "success",
	})
}

func ResponseError(ctx echo.Context, cause ...error) error {
	msg := "操作失败"
	code := http.StatusInternalServerError
	if len(cause) > 0 {
		c := cause[0]
		var t myerr.Error
		if errors.As(c, &t) {
			if t.StatusCode() > 0 {
				code = t.StatusCode()
			}
			msg = t.Error()
		}
	}
	content := map[string]string{
		"error": msg,
	}
	return ctx.JSON(code, content)
}

func fullHeaders(c echo.Context, headers map[string]string) {
	for k, v := range headers {
		c.Response().Header().Set(k, v)
	}
}

type Body struct {
	Msg  string      `json:"msg"`
	Data interface{} `json:"data"`
}

type PageData struct {
	Total int64       `json:"total"`
	List  interface{} `json:"list"`
}
