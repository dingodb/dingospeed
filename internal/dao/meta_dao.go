package dao

import (
	"encoding/hex"
	"fmt"

	"dingo-hfmirror/pkg/common"
	"dingo-hfmirror/pkg/config"
	"dingo-hfmirror/pkg/consts"
	"dingo-hfmirror/pkg/util"

	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

type MetaDao struct {
}

func NewMetaDao() *MetaDao {
	return &MetaDao{}
}

func (d *MetaDao) MetaGetGenerator(c echo.Context, repoType, org, repo, commit, method string) error {
	orgRepo := util.GetOrgRepo(org, repo)
	saveDir := fmt.Sprintf("%s/api/%s/%s/revision/%s", config.SysConfig.Repos(), repoType, orgRepo, commit)
	savePath := fmt.Sprintf("%s/%s", saveDir, fmt.Sprintf("meta_%s.json", method))
	err := util.MakeDirs(savePath)
	if err != nil {
		zap.S().Errorf("create %s dir err.%v", saveDir, err)
		return err
	}
	request := c.Request()
	authorization := request.Header.Get("authorization")
	// if util.FileExists(savePath) {
	// todo
	// } else {
	d.MetaProxyGenerator(c, repoType, org, repo, commit, method, authorization, savePath)
	// }
	return nil
}

func (d *MetaDao) MetaProxyGenerator(c echo.Context, repoType, org, repo, commit, method, authorization, savePath string) error {
	orgRepo := util.GetOrgRepo(org, repo)
	metaUrl := fmt.Sprintf("%s/api/%s/%s/revision/%s", config.SysConfig.GetHFURLBase(), repoType, orgRepo, commit)
	headers := map[string]string{}
	if authorization != "" {
		headers["authorization"] = authorization
	}
	if method == consts.RequestTypeHead {
		resp, err := util.Head(metaUrl, headers, consts.ApiTimeOut)
		if err != nil {
			zap.S().Errorf("head %s err.%v", metaUrl, err)
			return util.ErrorEntryNotFound(c)
		}
		respHeaders := make(map[string]string)
		for k, v := range resp.Header {
			if len(v) > 0 {
				respHeaders[k] = v[0]
			} else {
				respHeaders[k] = ""
			}
		}
		return util.ResponseHeaders(c, respHeaders)
	} else if method == consts.RequestTypeGet {
		resp, err := util.Get(metaUrl, headers, consts.ApiTimeOut)
		if err != nil {
			zap.S().Errorf("get %s err.%v", metaUrl, err)
			return util.ErrorEntryNotFound(c)
		}
		extractHeaders := util.ExtractHeaders(resp.Headers)
		var bodyStreamChan = make(chan []byte, 100)
		bodyStreamChan <- resp.Body
		close(bodyStreamChan)
		_, err = util.ResponseStream(c, repo, extractHeaders, bodyStreamChan, false)
		if err != nil {
			return err
		}
		if err = d.writeCacheRequest(savePath, resp.StatusCode, extractHeaders, resp.Body); err != nil {
			zap.S().Errorf("writeCacheRequest err.%v", err)
			return nil
		}
	}
	return nil
}

func (d *MetaDao) writeCacheRequest(savePath string, statusCode int, headers map[string]string, content []byte) error {
	cacheContent := common.CacheContent{
		StatusCode: statusCode,
		Headers:    headers,
		Content:    hex.EncodeToString(content),
	}
	return util.WriteDataToFile(savePath, cacheContent)
}
