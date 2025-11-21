package handler

import (
	"net/http"

	"dingospeed/internal/model/query"
	"dingospeed/internal/service"
	"dingospeed/pkg/consts"
	"dingospeed/pkg/util"

	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

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
	if createCacheJobReq.Org == "" && createCacheJobReq.Repo == "" {
		zap.S().Errorf("MetaProxyCommon org and repo is null")
		return util.ErrorRepoNotFound(c)
	}
	jobId, err := handler.cacheJobService.CreateCacheJob(c, createCacheJobReq)
	if err != nil {
		return util.ResponseError(c, err)
	}
	return util.ResponseData(c, util.Body{Msg: "success", Data: jobId})
}

func (handler *CacheJobHandler) StopCacheJobHandler(c echo.Context) error {
	jobStatusReq := new(query.JobStatusReq)
	if err := c.Bind(jobStatusReq); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "无效的 JSON 数据",
		})
	}
	err := handler.cacheJobService.StopCacheJob(jobStatusReq)
	if err != nil {
		return util.ResponseError(c, err)
	}
	return util.ResponseData(c, nil)
}

func (handler *CacheJobHandler) ResumeCacheJobHandler(c echo.Context) error {
	resumeJobReq := new(query.ResumeCacheJobReq)
	if err := c.Bind(resumeJobReq); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "无效的 JSON 数据",
		})
	}
	err := handler.cacheJobService.ResumeCacheJob(c, resumeJobReq)
	if err != nil {
		return util.ResponseError(c, err)
	}
	return util.ResponseData(c, nil)
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
