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

package handler

import (
	"fmt"
	"io"
	"net/http"
	"strconv"

	"dingospeed/internal/dao"
	"dingospeed/internal/service"

	"github.com/bytedance/sonic"
	"github.com/labstack/echo/v4"
)

// CacheAdminHandler 挂在上传服务上（强制绑回环的 8091），与上传接口共用同一层
// 隔离：这些接口会删数据，不能出现在对外的下载端口上。
//
// 这里没有身份校验——同机调用视为可信，隔离完全由“只监听回环”保证。跨机的
// 用户级权限由 spinfield 控制面负责，agent 只是无用户概念的执行器。
//
// 只有 JSON，没有页面：界面在 spinfield 控制台。
type CacheAdminHandler struct {
	cacheAdminService *service.CacheAdminService
}

func NewCacheAdminHandler(cacheAdminService *service.CacheAdminService) *CacheAdminHandler {
	return &CacheAdminHandler{cacheAdminService: cacheAdminService}
}

type deleteRequest struct {
	Items []dao.DeleteItem `json:"items"`
}

func (h *CacheAdminHandler) Summary(c echo.Context) error {
	result, err := h.cacheAdminService.Summary()
	if err != nil {
		return writeUploadError(c, "cache summary failed", err)
	}
	return c.JSON(http.StatusOK, result)
}

func (h *CacheAdminHandler) ListRepos(c echo.Context) error {
	result, err := h.cacheAdminService.ListRepos()
	if err != nil {
		return writeUploadError(c, "cache repo list failed", err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{"repos": result})
}

func (h *CacheAdminHandler) ListFiles(c echo.Context) error {
	result, err := h.cacheAdminService.ListFiles(parseCacheQuery(c))
	if err != nil {
		return writeUploadError(c, "cache file list failed", err)
	}
	return c.JSON(http.StatusOK, result)
}

func (h *CacheAdminHandler) ListOrphans(c echo.Context) error {
	result, err := h.cacheAdminService.ListOrphans(parseCacheQuery(c))
	if err != nil {
		return writeUploadError(c, "cache recycle list failed", err)
	}
	return c.JSON(http.StatusOK, result)
}

func (h *CacheAdminHandler) DeleteFiles(c echo.Context) error {
	items, err := readDeleteItems(c)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"code": "CACHE_INVALID_ARGUMENT", "error": err.Error()})
	}
	results, err := h.cacheAdminService.SoftDelete(items)
	if err != nil {
		return writeUploadError(c, "cache delete failed", err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{"results": results})
}

func (h *CacheAdminHandler) PurgeOrphans(c echo.Context) error {
	items, err := readDeleteItems(c)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"code": "CACHE_INVALID_ARGUMENT", "error": err.Error()})
	}
	results, err := h.cacheAdminService.PurgeOrphans(items)
	if err != nil {
		return writeUploadError(c, "cache purge failed", err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{"results": results})
}

func parseCacheQuery(c echo.Context) service.CacheQuery {
	return service.CacheQuery{
		RepoType: c.QueryParam("repoType"),
		OrgRepo:  c.QueryParam("orgRepo"),
		Source:   c.QueryParam("source"),
		Keyword:  c.QueryParam("keyword"),
		Sort:     c.QueryParam("sort"),
		Order:    c.QueryParam("order"),
		Page:     atoiOrZero(c.QueryParam("page")),
		PageSize: atoiOrZero(c.QueryParam("pageSize")),
	}
}

func atoiOrZero(raw string) int {
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0
	}
	return value
}

// readDeleteItems 限制请求体大小：这是个内网管理接口，但一次请求携带无上限的条目
// 会让一把仓库锁被持有任意久，阻塞正在进行的上传。
const maxDeleteBodyBytes = 4 << 20

func readDeleteItems(c echo.Context) ([]dao.DeleteItem, error) {
	body, err := io.ReadAll(io.LimitReader(c.Request().Body, maxDeleteBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxDeleteBodyBytes {
		return nil, fmt.Errorf("request body too large")
	}
	var req deleteRequest
	if err = sonic.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("body is not valid json: %v", err)
	}
	if len(req.Items) == 0 {
		return nil, fmt.Errorf("items is empty")
	}
	if len(req.Items) > 2000 {
		return nil, fmt.Errorf("too many items in one request: %d", len(req.Items))
	}
	return req.Items, nil
}
