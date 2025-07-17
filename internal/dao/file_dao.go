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
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"strings"
	"time"

	cache "dingospeed/internal/data"
	"dingospeed/internal/downloader"
	"dingospeed/pkg/common"
	"dingospeed/pkg/config"
	"dingospeed/pkg/consts"
	myerr "dingospeed/pkg/error"
	"dingospeed/pkg/util"

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
	if config.SysConfig.Cache.Enabled {
		cache.InitCache() // 初始化缓存
	}
	return &FileDao{}
}

func (f *FileDao) CheckCommitHf(repoType, org, repo, commit, authorization string) (int, error) {
	resp, err := f.remoteRequestMeta(consts.RequestTypeHead, repoType, org, repo, commit, authorization)
	if err != nil {
		zap.S().Errorf("head call meta %s/%s/%s error.%v", org, repo, commit, err)
		return http.StatusInternalServerError, err
	}
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusTemporaryRedirect {
		return resp.StatusCode, nil
	}
	zap.S().Errorf("CheckCommitHf statusCode:%d", resp.StatusCode)
	return resp.StatusCode, myerr.New("request commit err")
}

// 若为离线或在线请求失败，将进行本地仓库查找。

func (f *FileDao) GetCommitHf(repoType, org, repo, commit, authorization string) (string, error) {
	if !config.SysConfig.Online() {
		return f.getCommitHfOffline(repoType, org, repo, commit)
	}
	resp, err := f.remoteRequestMeta(consts.RequestTypeGet, repoType, org, repo, commit, authorization)
	if err != nil {
		zap.S().Errorf("get call meta %s/%s/%s error.%v", org, repo, commit, err)
		return f.getCommitHfOffline(repoType, org, repo, commit)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusTemporaryRedirect {
		return f.getCommitHfOffline(repoType, org, repo, commit)
	}
	// 保存到文件
	// orgRepo := util.GetOrgRepo(org, repo)
	// apiDir := fmt.Sprintf("%s/api/%s/%s/revision/%s", config.SysConfig.Repos(), repoType, orgRepo, commit)
	// apiMetaPath := fmt.Sprintf("%s/%s", apiDir, fmt.Sprintf("meta_get.json"))
	// if exist := util.FileExists(apiMetaPath); !exist {
	// 	err = util.MakeDirs(apiMetaPath)
	// 	if err != nil {
	// 		zap.S().Errorf("create %s dir err.%v", apiDir, err)
	// 		return f.getCommitHfOffline(repoType, org, repo, commit)
	// 	}
	// 	extractHeaders := resp.ExtractHeaders(resp.Headers)
	// 	if err = f.WriteCacheRequest(apiMetaPath, resp.StatusCode, extractHeaders, resp.Body); err != nil {
	// 		zap.S().Errorf("writeCacheRequest err.%v", err)
	// 		return f.getCommitHfOffline(repoType, org, repo, commit)
	// 	}
	// }
	var sha CommitHfSha
	if err = sonic.Unmarshal(resp.Body, &sha); err != nil {
		zap.S().Errorf("unmarshal content:%s, error:%v", string(resp.Body), err)
		return f.getCommitHfOffline(repoType, org, repo, commit)
	}
	return sha.Sha, nil
}

func (f *FileDao) remoteRequestMeta(method, repoType, org, repo, commit, authorization string) (*common.Response, error) {
	orgRepo := util.GetOrgRepo(org, repo)
	var reqUrl string
	if commit == "" {
		reqUrl = fmt.Sprintf("%s/api/%s/%s", config.SysConfig.GetHFURLBase(), repoType, orgRepo)
	} else {
		reqUrl = fmt.Sprintf("%s/api/%s/%s/revision/%s", config.SysConfig.GetHFURLBase(), repoType, orgRepo, commit)
	}
	headers := map[string]string{}
	if authorization != "" {
		headers["authorization"] = authorization
	}
	return util.RetryRequest(func() (*common.Response, error) {
		if method == consts.RequestTypeHead {
			return util.Head(reqUrl, headers, config.SysConfig.GetReqTimeOut())
		} else if method == consts.RequestTypeGet {
			return util.Get(reqUrl, headers, config.SysConfig.GetReqTimeOut())
		} else {
			return nil, fmt.Errorf("request method err")
		}
	})
}

func (f *FileDao) getCommitHfOffline(repoType, org, repo, commit string) (string, error) {
	orgRepo := util.GetOrgRepo(org, repo)
	apiPath := fmt.Sprintf("%s/api/%s/%s/revision/%s/meta_get.json", config.SysConfig.Repos(), repoType, orgRepo, commit)
	if util.FileExists(apiPath) {
		cacheContent, err := f.ReadCacheRequest(apiPath)
		if err != nil {
			return "", err
		}
		var sha CommitHfSha
		if err = sonic.Unmarshal(cacheContent.OriginContent, &sha); err != nil {
			zap.S().Errorf("unmarshal error.%v", err)
			return "", myerr.Wrap("Unmarshal err", err)
		}
		return sha.Sha, nil
	}
	return "", myerr.New(fmt.Sprintf("apiPath file not exist, %s", apiPath))
}

func (f *FileDao) FileGetGenerator(c echo.Context, repoType, orgRepo, commit, fileName, method string) error {
	var hfUrl string
	if repoType == "models" {
		hfUrl = fmt.Sprintf("%s/%s/resolve/%s/%s", config.SysConfig.GetHFURLBase(), orgRepo, commit, fileName)
	} else {
		hfUrl = fmt.Sprintf("%s/%s/%s/resolve/%s/%s", config.SysConfig.GetHFURLBase(), repoType, orgRepo, commit, fileName)
	}
	reqHeaders := map[string]string{}
	for k, _ := range c.Request().Header {
		if k == "host" {
			domain, _ := util.GetDomain(hfUrl)
			reqHeaders[strings.ToLower(k)] = domain
		} else {
			reqHeaders[strings.ToLower(k)] = c.Request().Header.Get(k)
		}
	}
	authorization := reqHeaders["authorization"]
	// _file_realtime_stream
	pathsInfos, err := f.pathsInfoGenerator(repoType, orgRepo, commit, authorization, []string{fileName}, "post")
	if err != nil {
		if e, ok := err.(myerr.Error); ok {
			zap.S().Errorf("pathsInfoGenerator code:%d, err:%v", e.StatusCode(), err)
			return util.ErrorEntryUnknown(c, e.StatusCode(), e.Error())
		}
		zap.S().Errorf("pathsInfoGenerator err:%v", err)
		return util.ErrorProxyError(c)
	}
	if len(pathsInfos) == 0 {
		zap.S().Errorf("pathsInfos is null. repo:%s, commit:%s, fileName:%s", orgRepo, commit, fileName)
		return util.ErrorEntryNotFound(c)
	}
	if len(pathsInfos) != 1 {
		zap.S().Errorf("pathsInfos not equal to 1. repo:%s, commit:%s, fileName:%s", orgRepo, commit, fileName)
		return util.ErrorProxyTimeout(c)
	}
	respHeaders := map[string]string{}
	pathInfo := pathsInfos[0]
	if pathInfo.Type == "directory" {
		zap.S().Warnf("repo:%s, commit:%s, fileName:%s is directory", orgRepo, commit, fileName)
		return util.ErrorEntryNotFound(c)
	}
	var startPos, endPos int64
	if pathInfo.Size > 0 { // There exists a file of size 0
		var headRange = reqHeaders["range"]
		if headRange == "" {
			headRange = fmt.Sprintf("bytes=%d-%d", 0, pathInfo.Size-1)
		}
		startPos, endPos = parseRangeParams(headRange, pathInfo.Size)
		endPos = endPos + 1
	} else if pathInfo.Size == 0 {
		zap.S().Warnf("file %s size: %d", fileName, pathInfo.Size)
	}
	respHeaders["content-length"] = util.Itoa(endPos - startPos)
	if commit != "" {
		respHeaders[strings.ToLower(consts.HUGGINGFACE_HEADER_X_REPO_COMMIT)] = commit
	}
	var etag string
	if pathInfo.Lfs.Oid != "" {
		etag = pathInfo.Lfs.Oid
	} else {
		etag = pathInfo.Oid
	}
	respHeaders["etag"] = etag
	blobsDir := fmt.Sprintf("%s/files/%s/%s/blobs", config.SysConfig.Repos(), repoType, orgRepo)
	blobsFile := fmt.Sprintf("%s/%s", blobsDir, etag)
	filesDir := fmt.Sprintf("%s/files/%s/%s/resolve/%s", config.SysConfig.Repos(), repoType, orgRepo, commit)
	filesPath := fmt.Sprintf("%s/%s", filesDir, fileName)
	if err = f.constructBlobsAndFileFile(c, blobsFile, filesPath); err != nil {
		return err
	}
	if method == consts.RequestTypeHead {
		return util.ResponseHeaders(c, respHeaders)
	} else if method == consts.RequestTypeGet {
		return f.FileChunkGet(c, hfUrl, blobsFile, filesPath, orgRepo, fileName, authorization, pathInfo.Size, startPos, endPos, respHeaders)
	} else {
		return util.ErrorMethodError(c)
	}
}

func (f *FileDao) constructBlobsAndFileFile(c echo.Context, blobsFile, filesPath string) (err error) {
	if err = util.MakeDirs(blobsFile); err != nil {
		zap.S().Errorf("create %s dir err.%v", blobsFile, err)
		err = util.ErrorProxyError(c)
		return
	}
	if err = util.MakeDirs(filesPath); err != nil {
		zap.S().Errorf("create %s dir err.%v", filesPath, err)
		err = util.ErrorProxyError(c)
		return
	}
	if exist := util.FileExists(filesPath); exist {
		if b, localErr := util.IsSymlink(filesPath); localErr != nil {
			zap.S().Errorf("IsSymlink %s err.%v", filesPath, localErr)
			err = util.ErrorProxyError(c)
			return
		} else {
			if !b {
				zap.S().Infof("old data transfer, from %s to %s", filesPath, blobsFile)
				if blobFileExist := util.FileExists(blobsFile); blobFileExist {
					if err = util.DeleteFile(filesPath); err != nil {
						err = util.ErrorProxyError(c)
						return
					}
				} else {
					util.ReName(filesPath, blobsFile)
				}
				if err = util.CreateSymlinkIfNotExists(blobsFile, filesPath); err != nil {
					zap.S().Errorf("filesPath:%s is not link", filesPath)
					err = util.ErrorProxyError(c)
					return
				}
			}
		}
	} else {
		if err = util.CreateSymlinkIfNotExists(blobsFile, filesPath); err != nil {
			zap.S().Errorf("filesPath:%s is not link", filesPath)
			err = util.ErrorProxyError(c)
			return
		}
	}
	return
}

func (f *FileDao) pathsInfoGenerator(repoType, orgRepo, commit, authorization string, paths []string, method string) ([]common.PathsInfo, error) {
	remoteReqFilePathMap := make(map[string]string, 0)
	ret := make([]common.PathsInfo, 0)
	for _, pathFileName := range paths {
		apiDir := fmt.Sprintf("%s/api/%s/%s/paths-info/%s/%s", config.SysConfig.Repos(), repoType, orgRepo, commit, pathFileName)
		apiPathInfoPath := fmt.Sprintf("%s/%s", apiDir, fmt.Sprintf("paths-info_%s.json", method))
		hitCache := util.FileExists(apiPathInfoPath)
		if hitCache {
			cacheContent, err := f.ReadCacheRequest(apiPathInfoPath)
			if err != nil {
				zap.S().Errorf("ReadCacheRequest err.%v", err)
				continue
			}
			if cacheContent.Version != consts.VersionSnapshot {
				remoteReqFilePathMap[pathFileName] = apiPathInfoPath
				continue
			}
			pathsInfos := make([]common.PathsInfo, 0)
			err = sonic.Unmarshal(cacheContent.OriginContent, &pathsInfos)
			if err != nil {
				zap.S().Errorf("pathsInfo Unmarshal err.%v", err)
				continue
			}
			if cacheContent.StatusCode == http.StatusOK {
				ret = append(ret, pathsInfos...)
			}
		} else {
			remoteReqFilePathMap[pathFileName] = apiPathInfoPath
		}
	}
	if len(remoteReqFilePathMap) > 0 {
		filePaths := make([]string, 0)
		for k := range remoteReqFilePathMap {
			filePaths = append(filePaths, k)
		}
		pathsInfoUrl := fmt.Sprintf("%s/api/%s/%s/paths-info/%s", config.SysConfig.GetHFURLBase(), repoType, orgRepo, commit)
		response, err := f.pathsInfoProxy(pathsInfoUrl, authorization, filePaths)
		if err != nil {
			zap.S().Errorf("req %s err.%v", pathsInfoUrl, err)
			return nil, myerr.NewAppendCode(http.StatusInternalServerError, fmt.Sprintf("%v", err))
		}
		if response.StatusCode != http.StatusOK {
			var errorResp common.ErrorResp
			if len(response.Body) > 0 {
				err = sonic.Unmarshal(response.Body, &errorResp)
				if err != nil {
					return nil, myerr.NewAppendCode(response.StatusCode, fmt.Sprintf("response code %d, %v", response.StatusCode, err))
				}
			}
			return nil, myerr.NewAppendCode(response.StatusCode, errorResp.Error)
		}
		remoteRespPathsInfos := make([]common.PathsInfo, 0)
		err = sonic.Unmarshal(response.Body, &remoteRespPathsInfos)
		if err != nil {
			zap.S().Errorf("req %s remoteRespPathsInfos Unmarshal err.%v", pathsInfoUrl, err)
			return nil, myerr.NewAppendCode(http.StatusInternalServerError, fmt.Sprintf("%v", err))
		}
		for _, item := range remoteRespPathsInfos {
			// 对单个文件pathsInfo做存储
			if apiPath, ok := remoteReqFilePathMap[item.Path]; ok {
				if err = util.MakeDirs(apiPath); err != nil {
					zap.S().Errorf("create %s dir err.%v", apiPath, err)
					continue
				}
				if item.Type == "file" {
					b, _ := sonic.Marshal([]common.PathsInfo{item}) // 转成单个文件的切片
					if err = f.WriteCacheRequest(apiPath, response.StatusCode, response.ExtractHeaders(response.Headers), b); err != nil {
						zap.S().Errorf("WriteCacheRequest err.%s,%v", apiPath, err)
						continue
					}
				}
			}
		}
		ret = append(ret, remoteRespPathsInfos...)
	}
	return ret, nil
}

func (f *FileDao) pathsInfoProxy(targetUrl, authorization string, filePaths []string) (*common.Response, error) {
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
	return util.RetryRequest(func() (*common.Response, error) {
		return util.Post(targetUrl, "application/json", jsonData, headers)
	})
}

func (f *FileDao) FileChunkGet(c echo.Context, hfUrl, blobsFile, filesPath, orgRepo, fileName, authorization string, fileSize, startPos, endPos int64, respHeaders map[string]string) error {
	responseChan := make(chan []byte, config.SysConfig.Download.RespChanSize)
	source := util.Itoa(c.Get(consts.PromSource))
	bgCtx := context.WithValue(c.Request().Context(), consts.PromSource, source)
	ctx, cancel := context.WithCancel(bgCtx)
	defer func() {
		cancel()
	}()
	go downloader.FileDownload(ctx, hfUrl, blobsFile, filesPath, orgRepo, fileName, authorization, fileSize, startPos, endPos, responseChan)
	if err := util.ResponseStream(c, fmt.Sprintf("%s/%s", orgRepo, fileName), respHeaders, responseChan); err != nil {
		zap.S().Warnf("FileChunkGet stream err.%v", err)
		return util.ErrorProxyTimeout(c)
	}
	return nil
}

func (f *FileDao) WhoamiV2Generator(c echo.Context) error {
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

func (f *FileDao) WriteCacheRequest(apiPath string, statusCode int, headers map[string]string, content []byte) error {
	cacheContent := common.CacheContent{
		Version:    consts.VersionSnapshot,
		StatusCode: statusCode,
		Headers:    headers,
		Content:    hex.EncodeToString(content),
	}
	return util.WriteDataToFile(apiPath, cacheContent)
}

func (f *FileDao) ReadCacheRequest(apiPath string) (*common.CacheContent, error) {
	cacheContent := common.CacheContent{}
	bytes, err := util.ReadFileToBytes(apiPath)
	if err != nil {
		return nil, myerr.Wrap("ReadFileToBytes err.", err)
	}
	if err = sonic.Unmarshal(bytes, &cacheContent); err != nil {
		return nil, err
	}
	decodeByte, err := hex.DecodeString(cacheContent.Content)
	if err != nil {
		return nil, myerr.Wrap("DecodeString err.", err)
	}
	cacheContent.OriginContent = decodeByte
	return &cacheContent, nil
}

func (f *FileDao) ReposGenerator(c echo.Context) error {
	reposPath := config.SysConfig.Repos()

	datasets, _ := filepath.Glob(filepath.Join(reposPath, "api/datasets/*/*"))
	datasetsRepos := util.ProcessPaths(datasets)

	models, _ := filepath.Glob(filepath.Join(reposPath, "api/models/*/*"))
	modelsRepos := util.ProcessPaths(models)

	spaces, _ := filepath.Glob(filepath.Join(reposPath, "api/spaces/*/*"))
	spacesRepos := util.ProcessPaths(spaces)

	return c.Render(http.StatusOK, "repos.html", map[string]interface{}{
		"datasets_repos": datasetsRepos,
		"models_repos":   modelsRepos,
		"spaces_repos":   spacesRepos,
	})
}

func parseRangeParams(fileRange string, fileSize int64) (int64, int64) {
	if strings.Contains(fileRange, "/") {
		split := strings.SplitN(fileRange, "/", 2)
		fileRange = split[0]
	}
	if strings.HasPrefix(fileRange, "bytes=") {
		fileRange = fileRange[6:]
	}
	parts := strings.Split(fileRange, "-")
	if len(parts) != 2 {
		panic("file range err.")
	}
	var startPos, endPos int64
	if len(parts[0]) != 0 {
		startPos = util.Atoi64(parts[0])
	} else {
		startPos = 0
	}
	if len(parts[1]) != 0 {
		endPos = util.Atoi64(parts[1])
	} else {
		endPos = fileSize - 1
	}
	return startPos, endPos
}
