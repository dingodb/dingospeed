package handler

import (
	"errors"
	"net/http"
	"os"
	"strconv"

	"dingospeed/internal/model/query"
	"dingospeed/internal/service"
	"dingospeed/pkg/consts"
	"dingospeed/pkg/util"

	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

func (handler *CacheJobHandler) CacheJobStatus(c echo.Context) error {
	id, err := strconv.ParseInt(c.QueryParam("id"), 10, 64)
	if err != nil || id <= 0 {
		return echo.NewHTTPError(400, "invalid job id")
	}
	status, err := handler.cacheJobService.CacheJobStatus(id)
	if err != nil {
		return repositoryHTTPError(err)
	}
	return c.JSON(200, status)
}

type CacheJobHandler struct {
	cacheJobService *service.CacheJobService
}

func NewCacheJobHandler(cacheJobService *service.CacheJobService) *CacheJobHandler {
	return &CacheJobHandler{
		cacheJobService: cacheJobService,
	}
}

func (handler *CacheJobHandler) CreateCacheJobHandler(c echo.Context) error {
	createCacheJobReq := new(query.CreateCacheJobReq)
	if err := c.Bind(createCacheJobReq); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "无效的 JSON 数据",
		})
	}
	if _, ok := consts.RepoTypesMapping[createCacheJobReq.Datatype]; !ok {
		zap.S().Errorf("MetaProxyCommon repoType:%s is not exist RepoTypesMapping", createCacheJobReq.Datatype)
		return util.ErrorPageNotFound(c)
	}
	if createCacheJobReq.Namespace == "" && createCacheJobReq.Repo == "" {
		zap.S().Errorf("MetaProxyCommon org and repo is null")
		return util.ErrorRepoNotFound(c)
	}
	result, err := handler.cacheJobService.CreateCacheJobResult(c, createCacheJobReq)
	if err != nil {
		return util.ResponseError(c, err)
	}
	return c.JSON(http.StatusOK, result)
}

func (handler *CacheJobHandler) StopCacheJobHandler(c echo.Context) error {
	jobStatusReq := new(query.JobStatusReq)
	if err := c.Bind(jobStatusReq); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "无效的 JSON 数据",
		})
	}
	err := handler.cacheJobService.StopCacheJob(jobStatusReq)
	return handler.cacheActionResponse(c, jobStatusReq.Id, err)
}

func (handler *CacheJobHandler) ResumeCacheJobHandler(c echo.Context) error {
	resumeJobReq := new(query.ResumeCacheJobReq)
	if err := c.Bind(resumeJobReq); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "无效的 JSON 数据",
		})
	}
	err := handler.cacheJobService.ResumeCacheJob(c, resumeJobReq)
	return handler.cacheActionResponse(c, resumeJobReq.Id, err)
}

func (handler *CacheJobHandler) RealtimeCacheJobHandler(c echo.Context) error {
	realtimeReq := new(query.RealtimeReq)
	if err := c.Bind(realtimeReq); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "无效的 JSON 数据",
		})
	}
	resp := handler.cacheJobService.RealtimeCacheJob(realtimeReq)
	return util.ResponseData(c, resp)
}

func (handler *CacheJobHandler) cacheActionResponse(c echo.Context, id int64, err error) error {
	if err != nil {
		var conflict *service.CacheJobConflict
		if errors.As(err, &conflict) {
			return c.JSON(409, map[string]any{"error": conflict.Code, "code": conflict.Code, "activeJobId": conflict.ActiveJobID})
		}
		return repositoryHTTPError(err)
	}
	status, err := handler.cacheJobService.CacheJobStatus(id)
	if errors.Is(err, os.ErrNotExist) {
		return util.ResponseData(c, nil)
	} // Legacy mount jobs have no preheat snapshot.
	if err != nil {
		return repositoryHTTPError(err)
	}
	code := http.StatusOK
	if status.State == "pausing" || status.State == "canceling" || status.State == "resuming" {
		code = http.StatusAccepted
	}
	return c.JSON(code, status)
}
