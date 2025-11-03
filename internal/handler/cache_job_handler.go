package handler

import (
	"net/http"

	"dingospeed/internal/model/query"
	"dingospeed/internal/service"
	"dingospeed/pkg/consts"
	myerr "dingospeed/pkg/error"
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
	job := new(query.CacheJobQuery)
	if err := c.Bind(job); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "无效的 JSON 数据",
		})
	}
	if _, ok := consts.RepoTypesMapping[job.Datatype]; !ok {
		zap.S().Errorf("MetaProxyCommon repoType:%s is not exist RepoTypesMapping", job.Datatype)
		return util.ErrorPageNotFound(c)
	}
	if job.Org == "" && job.Repo == "" {
		zap.S().Errorf("MetaProxyCommon org and repo is null")
		return util.ErrorRepoNotFound(c)
	}
	jobId, err := handler.cacheJobService.CreateCacheJob(c, job)
	if err != nil {
		if e, ok := err.(myerr.Error); ok {
			return util.ErrorEntryUnknown(c, e.StatusCode(), e.Error())
		}
		return util.ErrorProxyError(c)
	}
	return util.ResponseData(c, util.Body{Msg: "success", Data: jobId})
}

func (handler *CacheJobHandler) StopCacheJobHandler(c echo.Context) error {
	status := new(query.JobStatus)
	if err := c.Bind(status); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "无效的 JSON 数据",
		})
	}
	err := handler.cacheJobService.StopCacheJob(status)
	if err != nil {
		if e, ok := err.(myerr.Error); ok {
			return util.ErrorEntryUnknown(c, e.StatusCode(), e.Error())
		}
		return util.ErrorProxyError(c)
	}
	return util.ResponseData(c, nil)
}

func (handler *CacheJobHandler) ResumeCacheJobHandler(c echo.Context) error {
	cacheJobQuery := new(query.CacheJobQuery)
	if err := c.Bind(cacheJobQuery); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "无效的 JSON 数据",
		})
	}
	err := handler.cacheJobService.ResumeCacheJob(c, cacheJobQuery)
	if err != nil {
		if e, ok := err.(myerr.Error); ok {
			return util.ErrorEntryUnknown(c, e.StatusCode(), e.Error())
		}
		return util.ErrorProxyError(c)
	}
	return util.ResponseData(c, nil)
}
