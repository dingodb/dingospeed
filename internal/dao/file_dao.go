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

package dao

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"dingo-hfmirror/internal/downloader"
	"dingo-hfmirror/pkg/common"
	"dingo-hfmirror/pkg/config"
	"dingo-hfmirror/pkg/consts"
	"dingo-hfmirror/pkg/util"

	"github.com/bytedance/sonic"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

type CommitHfSha struct {
	Sha string `json:"sha"`
}

type FileDao struct {
}

func NewFileDao() *FileDao {
	return &FileDao{}
}

func (d *FileDao) CheckCommitHf(repoType, org, repo, commit, authorization string) bool {
	orgRepo := util.GetOrgRepo(org, repo)
	var reqUrl string
	if commit == "" {
		reqUrl = fmt.Sprintf("%s/api/%s/%s", config.SysConfig.GetHfEndpoint(), repoType, orgRepo)
	} else {
		reqUrl = fmt.Sprintf("%s/api/%s/%s/revision/%s", config.SysConfig.GetHfEndpoint(), repoType, orgRepo, commit)
	}
	headers := map[string]string{}
	if authorization != "" {
		headers["authorization"] = authorization
	}
	resp, err := util.Head(reqUrl, headers, consts.ApiTimeOut)
	if err != nil {
		zap.S().Errorf("call %s error.%v", reqUrl, err)
		return false
	}
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusTemporaryRedirect {
		return true
	}
	return false
}

func (d *FileDao) GetCommitHf(repoType, org, repo, commit, authorization string) string {
	if !config.SysConfig.Online() {
		return d.getCommitHfOffline(repoType, org, repo, commit)
	}
	orgRepo := util.GetOrgRepo(org, repo)
	var reqUrl string
	reqUrl = fmt.Sprintf("%s/api/%s/%s/revision/%s", config.SysConfig.GetHfEndpoint(), repoType, orgRepo, commit)
	headers := map[string]string{}
	if authorization != "" {
		headers["authorization"] = authorization
	}
	resp, err := util.Get(reqUrl, headers, consts.ApiTimeOut)
	if err != nil {
		zap.S().Errorf("call %s error.%v", reqUrl, err)
		return d.getCommitHfOffline(repoType, org, repo, commit)
	}
	var sha CommitHfSha
	if err = sonic.Unmarshal(resp, &sha); err != nil {
		zap.S().Errorf("unmarshal error.%v", err)
		return d.getCommitHfOffline(repoType, org, repo, commit)
	}
	return sha.Sha
}

func (d *FileDao) getCommitHfOffline(repoType, org, repo, commit string) string {
	// todo
	return ""
}

func (d *FileDao) FileGetGenerator(c echo.Context, repoType, org, repo, commit, fileName, method string) error {
	orgRepo := util.GetOrgRepo(org, repo)
	headsPath := fmt.Sprintf("%s/heads/%s/%s/resolve/%s/%s", config.SysConfig.Repos(), repoType, orgRepo, commit, fileName)
	filesDir := fmt.Sprintf("%s/files/%s/%s/resolve/%s", config.SysConfig.Repos(), repoType, orgRepo, commit)
	filesPath := fmt.Sprintf("%s/%s", filesDir, fileName)
	err := util.MakeDirs(headsPath)
	if err != nil {
		zap.S().Errorf("create %s dir err.%v", headsPath, err)
		return err
	}
	err = util.MakeDirs(filesPath)
	if err != nil {
		zap.S().Errorf("create %s dir err.%v", filesPath, err)
		return err
	}
	var hfUrl string
	if repoType == "models" {
		hfUrl = fmt.Sprintf("%s/%s/resolve/%s/%s", config.SysConfig.GetHfEndpoint(), orgRepo, commit, fileName)
	} else {
		hfUrl = fmt.Sprintf("%s/%s/%s/resolve/%s/%s", config.SysConfig.GetHfEndpoint(), repoType, orgRepo, commit, fileName)
	}
	headers := map[string]string{}
	request := c.Request()
	for k, _ := range request.Header {
		if k == "host" {
			domain, _ := util.GetDomain(hfUrl)
			headers[k] = domain
		} else {
			headers[k] = request.Header.Get(k)
		}
	}
	authorization := request.Header.Get("authorization")
	// _file_realtime_stream
	pathsInfos, err := d.pathsInfoGenerator(repoType, org, repo, commit, authorization, false, []string{fileName}, "post")
	if err != nil {
		return err
	}
	if len(pathsInfos) == 0 {
		return util.ErrorEntryNotFound(c)
	}
	if len(pathsInfos) != 1 {
		return util.ErrorProxyTimeout(c)
	}
	respHeaders := map[string]string{}
	pathInfo := pathsInfos[0]
	if pathInfo.Size == 0 {
		return util.ErrorProxyTimeout(c)
	}
	respHeaders["content-length"] = util.Itoa(pathInfo.Size)
	if commit != "" {
		respHeaders[strings.ToLower(consts.HUGGINGFACE_HEADER_X_REPO_COMMIT)] = commit
	}
	etag, err := d.getResourceEtag(hfUrl, authorization)
	if err != nil {
		return util.ErrorProxyTimeout(c)
	}
	respHeaders["etag"] = etag
	zap.S().Debugf("exec FileGetGenerator:commit:%s,etag:%s", commit, etag)
	if method == consts.RequestTypeHead {
		return util.ResponseHeaders(c, respHeaders)
	} else if method == consts.RequestTypeGet {
		return d.FileChunkGet(c, respHeaders, hfUrl, fileName, pathInfo.Size, filesDir)
	} else {
		return nil
	}
}

func (d *FileDao) pathsInfoGenerator(repoType, org, repo, commit, authorization string, overrideCache bool, paths []string, method string) ([]common.PathsInfo, error) {
	orgRepo := util.GetOrgRepo(org, repo)
	filePathMap := make(map[string]string, 0)
	pathsInfos := make([]common.PathsInfo, 0)
	for _, path := range paths {
		apiDir := fmt.Sprintf("%s/api/%s/%s/paths-info/%s/%s", config.SysConfig.Repos(), repoType, orgRepo, commit, path)
		apiPath := fmt.Sprintf("%s/%s", apiDir, fmt.Sprintf("paths-info_%s.json", method))
		hitCache := util.FileExists(apiPath)
		if hitCache && overrideCache {
			// todo 从缓存中获取pathsInfos
		} else {
			filePathMap[path] = apiPath
		}
	}
	if len(filePathMap) > 0 {
		filePaths := make([]string, 0)
		for k := range filePathMap {
			filePaths = append(filePaths, k)
		}
		pathsInfoUrl := fmt.Sprintf("%s/api/%s/%s/paths-info/%s", config.SysConfig.GetHfEndpoint(), repoType, orgRepo, commit)
		response, err := d.pathsInfoProxy(pathsInfoUrl, authorization, filePaths)
		if err != nil {
			zap.S().Errorf("req %s err.%v", pathsInfoUrl, err)
			return nil, err
		}
		err = sonic.Unmarshal(response.Body, &pathsInfos)
		if err != nil {
			return nil, err
		}
		for _, item := range pathsInfos {
			if apiPath, ok := filePathMap[item.Path]; ok {
				b, _ := sonic.Marshal(item)
				d.writeCacheRequest(apiPath, response.StatusCode, response.Headers, b)
			}
		}
	}
	return pathsInfos, nil
}

func (d *FileDao) writeCacheRequest(apiPath string, statusCode int, headers map[string]interface{}, content []byte) error {
	err := util.MakeDirs(apiPath)
	if err != nil {
		zap.S().Errorf("create %s dir err.%v", apiPath, err)
		return err
	}
	lowerCaseHeaders := make(map[string]interface{})
	for k, v := range headers {
		if strSlice, ok := v.([]string); ok {
			if len(strSlice) > 0 {
				lowerCaseHeaders[strings.ToLower(k)] = strSlice[0]
			}
		} else {
			lowerCaseHeaders[strings.ToLower(k)] = v
		}
	}
	m := map[string]interface{}{
		"status_code": statusCode,
		"headers":     lowerCaseHeaders,
		"content":     hex.EncodeToString(content),
	}
	jsonData, err := sonic.Marshal(m)
	if err != nil {
		return fmt.Errorf("JSON 编码出错: %w", err)
	}
	file, err := os.OpenFile(apiPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("打开文件出错: %w", err)
	}
	defer file.Close()
	_, err = file.Write(jsonData)
	if err != nil {
		return fmt.Errorf("写入文件出错: %w", err)
	}
	return nil
}

func (d *FileDao) pathsInfoProxy(targetUrl, authorization string, filePaths []string) (*common.Response, error) {
	data := map[string]interface{}{
		"paths": filePaths,
	}
	jsonData, err := sonic.Marshal(data)
	if err != nil {
		return nil, err
	}
	headers := map[string]string{}
	if authorization != "" {
		headers["authorization"] = authorization
	}
	return util.Post(targetUrl, "application/json", jsonData, headers)
}

func (d *FileDao) getResourceEtag(hfUrl, authorization string) (string, error) {
	// 计算 hfURL 的 SHA256 哈希值
	hash := sha256.Sum256([]byte(hfUrl))
	contentHash := hex.EncodeToString(hash[:])
	var retEtag string
	if !config.SysConfig.Online() {
		retEtag = fmt.Sprintf(`"%s-10"`, contentHash[:32])
	} else {
		etagHeaders := make(map[string]string)
		if authorization != "" {
			etagHeaders["authorization"] = authorization
		}
		resp, err := util.Head(hfUrl, etagHeaders, consts.ApiTimeOut)
		if err != nil {
			return "", err
		}
		if etag := resp.Header.Get("etag"); etag != "" {
			retEtag = etag
		} else {
			retEtag = fmt.Sprintf(`"%s-10"`, contentHash[:32])
		}
	}
	return retEtag, nil
}

func (d *FileDao) FileChunkGet(c echo.Context, headers map[string]string, hfUrl, fileName string, fileSize int64, fileDir string) error {
	// 1.针对每个文件获取一个下载器
	loader, err := downloader.GetDownLoader(downloader.FileInfo{
		FileName: fileName, FileSize: fileSize,
	}, fileDir)
	if err != nil {
		return util.ErrorProxyError(c)
	}
	loader.DownloadUrl = hfUrl
	if err = loader.DownloadFileBySlice(); err != nil {
		return util.ErrorProxyError(c)
	}
	loader.MergeDownloadFiles()
	return d.responseBlock(c, headers, loader.BlockData)
}

func (d *FileDao) responseBlock(c echo.Context, headers map[string]string, content <-chan []byte) error {
	c.Response().Header().Set("Content-Type", "text/event-stream")
	c.Response().Header().Set("Cache-Control", "no-cache")
	c.Response().Header().Set("Connection", "keep-alive")
	for k, v := range headers {
		c.Response().Header().Set(k, v)
	}
	c.Response().WriteHeader(http.StatusOK)
	flusher, ok := c.Response().Writer.(http.Flusher)
	if !ok {
		return c.String(http.StatusInternalServerError, "Streaming unsupported!")
	}
	for {
		select {
		case b, ok := <-content:
			if !ok {
				return nil
			}
			zap.S().Infof("resp back:%d", len(b))
			if _, err := c.Response().Write(b); err != nil {
				zap.S().Errorf("resp write err.%v", err)
				return err
			}
			flusher.Flush()
		}
	}
}

func (d *FileDao) WhoamiV2Generator(c echo.Context) error {
	newHeaders := make(http.Header)
	for k, vv := range c.Request().Header {
		lowerKey := strings.ToLower(k)
		if lowerKey == "host" {
			continue
		}
		newHeaders[lowerKey] = vv
	}

	targetURL, err := url.Parse(config.SysConfig.GetHFURLBase())
	if err != nil {
		zap.L().Error("Failed to parse base URL", zap.Error(err))
		return echo.NewHTTPError(http.StatusInternalServerError, "Internal Server Error")
	}
	targetURL.Path = path.Join(targetURL.Path, "/api/whoami-v2")

	// targetURL := "https://huggingface.co/api/whoami-v2"
	zap.S().Debugf("exec WhoamiV2Generator:targetURL:%s,host:%s", targetURL.String(), config.SysConfig.GetHfNetLoc())
	// Creating a proxy request
	req, err := http.NewRequest("GET", targetURL.String(), nil)
	if err != nil {
		zap.L().Error("Failed to create request", zap.Error(err))
		return echo.NewHTTPError(http.StatusInternalServerError, "Internal Server Error")
	}
	req.Header = newHeaders
	// req.Host = "huggingface.co"
	req.Host = config.SysConfig.GetHfNetLoc()

	client := &http.Client{
		Timeout: 10 * time.Second,
	}
	resp, err := client.Do(req)
	if err != nil {
		zap.L().Error("Failed to forward request", zap.Error(err))
		return echo.NewHTTPError(http.StatusBadGateway, "Bad Gateway")
	}
	defer resp.Body.Close()

	// Processing Response Headers
	responseHeaders := make(http.Header)
	for k, vv := range resp.Header {
		lowerKey := strings.ToLower(k)
		if lowerKey == "content-encoding" || lowerKey == "content-length" {
			continue
		}
		for _, v := range vv {
			responseHeaders.Add(lowerKey, v)
		}
	}

	// Setting the response header
	for k, vv := range responseHeaders {
		for _, v := range vv {
			c.Response().Header().Add(k, v)
		}
	}

	c.Response().WriteHeader(resp.StatusCode)

	// Streaming response content
	_, err = io.Copy(c.Response().Writer, resp.Body)
	if err != nil {
		zap.L().Error("Failed to stream response", zap.Error(err))
	}

	return nil
}
