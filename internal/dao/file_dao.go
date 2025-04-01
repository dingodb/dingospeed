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
	"path"
	"strings"
	"time"

	"dingo-hfmirror/internal/downloader"
	"dingo-hfmirror/pkg/common"
	"dingo-hfmirror/pkg/config"
	"dingo-hfmirror/pkg/consts"
	myerr "dingo-hfmirror/pkg/error"
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

func (f *FileDao) CheckCommitHf(repoType, org, repo, commit, authorization string) bool {
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
	resp, err := util.Head(reqUrl, headers, config.SysConfig.GetReqTimeOut())
	if err != nil {
		zap.S().Errorf("call %s error.%v", reqUrl, err)
		return false
	}
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusTemporaryRedirect {
		return true
	}
	return false
}

// 若为离线或在线请求失败，将进行本地仓库查找。

func (f *FileDao) GetCommitHf(repoType, org, repo, commit, authorization string) (string, error) {
	if !config.SysConfig.Online() {
		return f.getCommitHfOffline(repoType, org, repo, commit)
	}
	orgRepo := util.GetOrgRepo(org, repo)
	var reqUrl string
	reqUrl = fmt.Sprintf("%s/api/%s/%s/revision/%s", config.SysConfig.GetHFURLBase(), repoType, orgRepo, commit)
	headers := map[string]string{}
	if authorization != "" {
		headers["authorization"] = authorization
	}
	resp, err := util.Get(reqUrl, headers, config.SysConfig.GetReqTimeOut())
	if err != nil {
		zap.S().Errorf("call %s error.%v", reqUrl, err)
		return f.getCommitHfOffline(repoType, org, repo, commit)
	}
	var sha CommitHfSha
	if err = sonic.Unmarshal(resp.Body, &sha); err != nil {
		zap.S().Errorf("unmarshal error.%v", err)
		return f.getCommitHfOffline(repoType, org, repo, commit)
	}
	return sha.Sha, nil
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

func (f *FileDao) FileGetGenerator(c echo.Context, repoType, org, repo, commit, fileName, method string) error {
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
	pathsInfos, err := f.pathsInfoGenerator(repoType, org, repo, commit, authorization, []string{fileName}, "post")
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
	etag, err := f.getResourceEtag(hfUrl, authorization)
	if err != nil {
		return util.ErrorProxyTimeout(c)
	}
	respHeaders["etag"] = etag
	zap.S().Debugf("FileGetGenerator-fileName:%s,commit:%s,etag:%s", fileName, commit, etag)
	if method == consts.RequestTypeHead {
		return util.ResponseHeaders(c, respHeaders)
	} else if method == consts.RequestTypeGet {
		return f.FileChunkGet(c, respHeaders, reqHeaders, hfUrl, pathInfo.Size, fileName, filesPath)
	} else {
		return util.ErrorMethodError(c)
	}
}

func (f *FileDao) pathsInfoGenerator(repoType, org, repo, commit, authorization string, paths []string, method string) ([]common.PathsInfo, error) {
	orgRepo := util.GetOrgRepo(org, repo)
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
			return nil, err
		}
		remoteRespPathsInfos := make([]common.PathsInfo, 0)
		err = sonic.Unmarshal(response.Body, &remoteRespPathsInfos)
		if err != nil {
			return nil, err
		}
		for _, item := range remoteRespPathsInfos {
			// 对单个文件pathsInfo做存储
			if apiPath, ok := remoteReqFilePathMap[item.Path]; ok {
				if err = util.MakeDirs(apiPath); err != nil {
					zap.S().Errorf("create %s dir err.%v", apiPath, err)
					continue
				}
				b, _ := sonic.Marshal([]common.PathsInfo{item}) // 转成单个文件的切片
				if err = f.WriteCacheRequest(apiPath, response.StatusCode, response.ExtractHeaders(response.Headers), b); err != nil {
					zap.S().Errorf("WriteCacheRequest err.%s,%v", apiPath, err)
					continue
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
	return util.Post(targetUrl, "application/json", jsonData, headers)
}

func (f *FileDao) getResourceEtag(hfUrl, authorization string) (string, error) {
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
		resp, err := util.Head(hfUrl, etagHeaders, config.SysConfig.GetReqTimeOut())
		if err != nil {
			return "", err
		}
		if etag := resp.GetKey("etag"); etag != "" {
			retEtag = etag
		} else {
			retEtag = fmt.Sprintf(`"%s-10"`, contentHash[:32])
		}
	}
	return retEtag, nil
}

// func (f *FileDao) FileChunkGet(c echo.Context, headers map[string]string, hfUrl, fileName string, fileSize int64, fileDir string) error {
// 	// 1.针对每个文件获取一个下载器
// 	fInfo := downloader.FileInfo{
// 		FileName: fileName, FileSize: fileSize,
// 	}
// 	var startPos, endPos int64 = 0, fileSize - 1
// 	if v, ok := headers["range"]; ok {
// 		startPos, endPos = parseRangeParams(v, fileSize)
// 	}
// 	fInfo.StartPos = startPos
// 	fInfo.EndPos = endPos
// 	loader, err := downloader.GetDownLoader(fInfo, fileDir)
// 	if err != nil {
// 		return util.ErrorProxyError(c)
// 	}
// 	loader.DownloadUrl = hfUrl
// 	if err = loader.DownloadFileBySlice(); err != nil {
// 		return util.ErrorProxyError(c)
// 	}
// 	if err = loader.MergeDownloadFiles(); err != nil {
// 		return util.ErrorProxyTimeout(c)
// 	}
// 	if _, err = util.ResponseStream(c, fileName, headers, loader.BlockData, false); err != nil {
// 		return util.ErrorProxyTimeout(c)
// 	}
// 	return nil
// }

func (f *FileDao) FileChunkGet(c echo.Context, respHeaders, reqHeaders map[string]string, hfUrl string, fileSize int64, fileName, filesPath string) error {
	contentChan := make(chan []byte, 100)
	go downloader.FileDownload(hfUrl, filesPath, fileSize, reqHeaders, contentChan)
	if err := util.ResponseStream(c, fileName, respHeaders, contentChan); err != nil {
		zap.S().Errorf("FileChunkGet stream err.%v", err)
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
