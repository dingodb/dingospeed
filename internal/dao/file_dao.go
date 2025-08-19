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

	"dingospeed/internal/data"
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
	downloaderDao *DownloaderDao
	baseData      *data.BaseData
}

func NewFileDao(downloaderDao *DownloaderDao, baseData *data.BaseData) *FileDao {
	return &FileDao{downloaderDao: downloaderDao, baseData: baseData}
}

func (f *FileDao) CheckCommitHf(repoType, orgRepo, commit, authorization string) (int, error) {
	resp, err := f.RemoteRequestMeta(consts.RequestTypeHead, repoType, orgRepo, commit, authorization)
	if err != nil {
		zap.S().Errorf("head call meta %s/%s error.%v", orgRepo, commit, err)
		return http.StatusInternalServerError, err
	}
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusTemporaryRedirect {
		return resp.StatusCode, nil
	}
	zap.S().Errorf("CheckCommitHf statusCode:%d", resp.StatusCode)
	return resp.StatusCode, myerr.New("request commit err")
}

func (f *FileDao) GetFileCommitSha(c echo.Context, repoType, orgRepo, commit string) (string, error) {
	authorization := c.Request().Header.Get("authorization")
	if v, ok := f.baseData.Cache.Get(util.GetMetaRepoKey(orgRepo, commit)); ok {
		return v.(string), nil
	}
	var (
		commitSha string
		err       error
	)
	if config.SysConfig.Online() {
		code, sha, err := f.getCommitHfRemote(repoType, orgRepo, commit, authorization)
		if err == nil {
			if code != http.StatusOK && code != http.StatusTemporaryRedirect {
				return "", myerr.NewAppendCode(code, "code is invalid.")
			} else {
				commitSha = sha
			}
			f.baseData.Cache.SetDefault(util.GetMetaRepoKey(orgRepo, commit), commitSha)
			return commitSha, nil
		}
	}
	commitSha, err = f.GetCommitHfOffline(repoType, orgRepo, commit)
	if err != nil {
		zap.S().Errorf(" getFileCommitSha GetCommitHfOffline err.%v", err)
		return "", myerr.NewAppendCode(http.StatusNotFound, fmt.Sprintf("%s is not found", orgRepo))
	}
	f.baseData.Cache.SetDefault(util.GetMetaRepoKey(orgRepo, commit), commitSha)
	return commitSha, nil
}

// 若为离线或在线请求失败，将进行本地仓库查找。

func (f *FileDao) getCommitHfRemote(repoType, orgRepo, commit, authorization string) (int, string, error) {
	resp, err := f.RemoteRequestMeta(consts.RequestTypeGet, repoType, orgRepo, commit, authorization)
	if err != nil {
		zap.S().Errorf("get call meta %s/%s error.%v", orgRepo, commit, err)
		return http.StatusInternalServerError, "", err
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusTemporaryRedirect {
		return resp.StatusCode, "", nil
	}
	var sha CommitHfSha
	if err = sonic.Unmarshal(resp.Body, &sha); err != nil {
		zap.S().Errorf("unmarshal content:%s, error:%v", string(resp.Body), err)
		return http.StatusInternalServerError, "", err
	}
	return resp.StatusCode, sha.Sha, nil
}

func (f *FileDao) RemoteRequestMeta(method, repoType, orgRepo, commit, authorization string) (*common.Response, error) {
	var reqUri string
	if commit == "" {
		reqUri = fmt.Sprintf("/api/%s/%s", repoType, orgRepo)
	} else {
		reqUri = fmt.Sprintf("/api/%s/%s/revision/%s", repoType, orgRepo, commit)
	}
	headers := map[string]string{}
	if authorization != "" {
		headers["authorization"] = authorization
	}
	return util.RetryRequest(func() (*common.Response, error) {
		if method == consts.RequestTypeHead {
			return util.Head(reqUri, headers)
		} else if method == consts.RequestTypeGet {
			return util.Get(reqUri, headers)
		} else {
			return nil, fmt.Errorf("request method err")
		}
	})
}

func (f *FileDao) GetCommitHfOffline(repoType, orgRepo, commit string) (string, error) {
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
	var hfUri string
	if repoType == "models" {
		hfUri = fmt.Sprintf("/%s/resolve/%s/%s", orgRepo, commit, fileName)
	} else {
		hfUri = fmt.Sprintf("/%s/%s/resolve/%s/%s", repoType, orgRepo, commit, fileName)
	}
	authorization := c.Request().Header.Get("Authorization")
	// _file_realtime_stream
	pathsInfos, err := f.GetPathsInfo(repoType, orgRepo, commit, authorization, []string{fileName})
	if err != nil {
		if e, ok := err.(myerr.Error); ok {
			zap.S().Errorf("GetPathsInfo code:%d, err:%v", e.StatusCode(), err)
			return util.ErrorEntryUnknown(c, e.StatusCode(), e.Error())
		}
		zap.S().Errorf("GetPathsInfo err:%v", err)
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
	pathInfo := pathsInfos[0]
	if pathInfo.Type == "directory" {
		zap.S().Warnf("repo:%s, commit:%s, fileName:%s is directory", orgRepo, commit, fileName)
		return util.ErrorEntryNotFound(c)
	}
	var startPos, endPos int64
	if pathInfo.Size > 0 { // There exists a file of size 0
		var headRange = c.Request().Header.Get("Range")
		if headRange == "" {
			headRange = fmt.Sprintf("bytes=%d-%d", 0, pathInfo.Size-1)
		}
		startPos, endPos = parseRangeParams(headRange, pathInfo.Size)
		endPos = endPos + 1
	} else if pathInfo.Size == 0 {
		zap.S().Warnf("file %s size: %d", fileName, pathInfo.Size)
	}
	respHeaders := map[string]string{}
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
		preheat := c.Request().Header.Get("preheat")
		preheatFlag := preheat != ""
		taskParam := &downloader.TaskParam{
			TaskNo:        0,
			BlobsFile:     blobsFile,
			FileName:      fileName,
			FileSize:      pathInfo.Size,
			OrgRepo:       orgRepo,
			Authorization: authorization,
			Uri:           hfUri,
			DataType:      repoType,
			Etag:          etag,
			Preheat:       preheatFlag,
		}
		return f.FileChunkGet(c, taskParam, startPos, endPos, respHeaders)
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
		err = util.CreateFileIfNotExist(blobsFile)
		if err != nil {
			zap.S().Errorf("create filesPath:%s err", filesPath)
			err = util.ErrorProxyError(c)
			return err
		}
		if err = util.CreateSymlinkIfNotExists(blobsFile, filesPath); err != nil {
			zap.S().Errorf("filesPath:%s is not link", filesPath)
			err = util.ErrorProxyError(c)
			return
		}
	}
	return
}

func (f *FileDao) GetPathsInfo(repoType, orgRepo, commit, authorization string, pathFileNames []string) ([]common.PathsInfo, error) {
	remoteReqFilePathMap := make(map[string]string, 0)
	ret := make([]common.PathsInfo, 0)
	for _, pathFileName := range pathFileNames {
		apiPathInfoPath := fmt.Sprintf("%s/api/%s/%s/paths-info/%s/%s/paths-info_post.json", config.SysConfig.Repos(), repoType, orgRepo, commit, pathFileName)
		hitCache := util.FileExists(apiPathInfoPath)
		if hitCache {
			if cacheContent, err := f.ReadCacheRequest(apiPathInfoPath); err != nil { // 若存在缓存文件，却读取失败，将会在线请求。
				zap.S().Errorf("ReadCacheRequest err.%v", err)
			} else {
				if cacheContent.Version != consts.VersionSnapshot {
					// 若是老版本，需要重新请求pathsInfo数据
				} else {
					pathsInfos := make([]common.PathsInfo, 0)
					if err = sonic.Unmarshal(cacheContent.OriginContent, &pathsInfos); err != nil {
						zap.S().Errorf("pathsInfo Unmarshal err.%v", err)
					} else if cacheContent.StatusCode == http.StatusOK {
						ret = append(ret, pathsInfos...)
						continue
					}
				}
			}
		}
		// 若命中缓存读取失败，将执行在线
		remoteReqFilePathMap[pathFileName] = apiPathInfoPath
	}
	if len(remoteReqFilePathMap) > 0 {
		filePaths := make([]string, 0)
		for k := range remoteReqFilePathMap {
			filePaths = append(filePaths, k)
		}
		pathsInfoUri := fmt.Sprintf("/api/%s/%s/paths-info/%s", repoType, orgRepo, commit)
		response, err := f.pathsInfoProxy(pathsInfoUri, authorization, filePaths)
		if err != nil {
			zap.S().Errorf("req %s err.%v", pathsInfoUri, err)
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
			zap.S().Errorf("req %s remoteRespPathsInfos Unmarshal err.%v", pathsInfoUri, err)
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

func (f *FileDao) pathsInfoProxy(targetUri, authorization string, filePaths []string) (*common.Response, error) {
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
		return util.Post(targetUri, "application/json", jsonData, headers)
	})
}

func (f *FileDao) FileChunkGet(c echo.Context, taskParam *downloader.TaskParam, startPos, endPos int64, respHeaders map[string]string) error {
	responseChan := make(chan []byte, config.SysConfig.Download.RespChanSize)
	source := util.Itoa(c.Get(consts.PromSource))
	bgCtx := context.WithValue(c.Request().Context(), consts.PromSource, source)
	ctx, cancel := context.WithCancel(bgCtx)
	defer func() {
		cancel()
	}()
	var isInnerRequest bool
	if value := c.Request().Header.Get(consts.RequestSourceInner); value == "1" {
		isInnerRequest = true
	}
	taskParam.Context = ctx
	taskParam.ResponseChan = responseChan
	taskParam.Cancel = cancel
	go f.downloaderDao.FileDownload(startPos, endPos, isInnerRequest, taskParam)
	if err := util.ResponseStream(c, fmt.Sprintf("%s/%s", taskParam.OrgRepo, taskParam.FileName), respHeaders, responseChan); err != nil {
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
