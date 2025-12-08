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

package service

import (
	"fmt"
	"io"
	"sort"

	"dingospeed/internal/dao"
	"dingospeed/pkg/common"
	"dingospeed/pkg/config"
	"dingospeed/pkg/consts"
	"dingospeed/pkg/util"

	"github.com/bytedance/sonic"
	"github.com/labstack/echo/v4"
	"github.com/labstack/gommon/log"
	"go.uber.org/zap"
)

type MetaService struct {
	fileDao *dao.FileDao
	metaDao *dao.MetaDao
}

func NewMetaService(fileDao *dao.FileDao, metaDao *dao.MetaDao) *MetaService {
	return &MetaService{
		fileDao: fileDao,
		metaDao: metaDao,
	}
}

func (m *MetaService) GetMetadata(repoType, orgRepo, revision, method, authorization string) (*common.CacheContent, error) {
	zap.S().Debugf("GetMetadata:%s/%s/%s/%s", repoType, orgRepo, revision, method)
	return m.metaDao.GetMetadata(repoType, orgRepo, revision, method, authorization)
}

func (m *MetaService) WhoamiV2(c echo.Context) error {
	err := m.metaDao.WhoamiV2Generator(c)
	return err
}

func (m *MetaService) Repos(c echo.Context) error {
	err := m.metaDao.ReposGenerator(c)
	return err
}

func (m *MetaService) RepoRefs(c echo.Context, repoType, org, repo string) error {
	orgRepo := util.GetOrgRepo(org, repo)
	zap.S().Debugf("RepoRefs:%s/%s", repoType, orgRepo)
	if _, ok := consts.RepoTypesMapping[repoType]; !ok {
		zap.S().Errorf("RepoRefs repoType:%s is not exist RepoTypesMapping", repoType)
		return util.ErrorPageNotFound(c)
	}
	if org == "" && repo == "" {
		zap.S().Errorf("RepoRefs org and repo is null")
		return util.ErrorRepoNotFound(c)
	}
	authorization := c.Request().Header.Get("authorization")
	localRefsDir := fmt.Sprintf("%s/api/%s/%s/refs", config.SysConfig.Repos(), repoType, orgRepo)
	localRefsPath := fmt.Sprintf("%s/%s", localRefsDir, fmt.Sprintf("refs_get.json"))
	err := util.MakeDirs(localRefsPath)
	if err != nil {
		zap.S().Errorf("create %s dir err.%v", localRefsPath, err)
		return util.ErrorProxyError(c)
	}
	var cacheContent *common.CacheContent
	if !config.SysConfig.Online() && util.FileExists(localRefsPath) {
		cacheContent, err = m.fileDao.ReadCacheRequest(localRefsPath)
		if err != nil {
			zap.S().Errorf("ReadCacheRequest %s dir err.%v", localRefsPath, err)
			return util.ErrorProxyError(c)
		}
	} else {
		resp, err := m.metaDao.RepoRefs(repoType, orgRepo, authorization)
		if err != nil {
			zap.S().Errorf("get repo refs err.%v", err)
			return util.ErrorProxyError(c)
		}
		extractHeaders := resp.ExtractHeaders(resp.Headers)
		if err = m.fileDao.WriteCacheRequest(localRefsPath, resp.StatusCode, extractHeaders, resp.Body); err != nil {
			zap.S().Errorf("writeCacheRequest err.%v", err)
			return util.ErrorProxyError(c)
		}
		cacheContent = &common.CacheContent{
			Headers:       extractHeaders,
			OriginContent: resp.Body,
		}
	}
	var bodyStreamChan = make(chan []byte, consts.RespChanSize)
	bodyStreamChan <- cacheContent.OriginContent
	close(bodyStreamChan)
	return util.ResponseStream(c, orgRepo, cacheContent.Headers, bodyStreamChan)
}

func (m *MetaService) ForwardToNewSite(c echo.Context) error {
	zap.S().Infof("ForwardToNewSite url:%s", c.Request().URL.Path)
	resp, err := m.metaDao.ForwardRefs(c)
	if err != nil {
		zap.S().Errorf("forward request refs err.%v", err)
		return util.ErrorProxyError(c)
	}
	defer resp.Body.Close()
	response := c.Response()
	for k, v := range resp.Header {
		response.Header()[k] = v
	}
	response.WriteHeader(resp.StatusCode)
	_, err = io.Copy(response, resp.Body)
	if err != nil {
		return util.ErrorProxyError(c)
	}
	return nil
}

func (m *MetaService) RepositoryFiles(repoType, orgRepo, commit, filePath string) ([]*FileDescribe, error) {
	pathsInfoShaDir := fmt.Sprintf("%s/api/%s/%s/paths-info/%s", config.SysConfig.Repos(), repoType, orgRepo, commit)
	if filePath != "" {
		pathsInfoShaDir += fmt.Sprintf("/%s", filePath)
	}
	downloadLinkRoot := fmt.Sprintf("%s/%s/%s/resolve/%s", config.SysConfig.Scheduler.PublicDomain, repoType, orgRepo, commit)
	if b := util.FileExists(pathsInfoShaDir); !b {
		log.Warnf("pathsInfoShaDir is not exitst.%s", pathsInfoShaDir)
		return nil, fmt.Errorf("file not exists")
	}
	if files, err := util.ReadDir(pathsInfoShaDir); err != nil {
		log.Warnf("ReadDir %s , %s error.%v", orgRepo, pathsInfoShaDir, err)
		return nil, err
	} else {
		fileDescribes := make([]*FileDescribe, 0)
		filePathName := ""
		for _, item := range files {
			fileDescribe, err := m.analysisFile(pathsInfoShaDir, filePath, item)
			if err != nil {
				zap.S().Errorf("analysisFile err.%v", err)
				continue
			}
			if !fileDescribe.IsDir {
				if filePath != "" {
					filePathName = fmt.Sprintf("%s/%s", filePath, item)
				} else {
					filePathName = item
				}
				fileDescribe.Link = fmt.Sprintf("%s/%s", downloadLinkRoot, filePathName)
			}
			fileDescribes = append(fileDescribes, fileDescribe)
		}
		sortNodes(fileDescribes)
		return fileDescribes, nil
	}
}

func sortNodes(nodes []*FileDescribe) {
	sort.Slice(nodes, func(i, j int) bool {
		// 目录排在文件前面
		if nodes[i].IsDir && !nodes[j].IsDir {
			return true
		}
		if !nodes[i].IsDir && nodes[j].IsDir {
			return false
		}
		// 同是目录或同是文件，按名称正序排列
		return nodes[i].Name < nodes[j].Name
	})
}

func (m *MetaService) analysisFile(pathInfoShaDir, filePath, fileName string) (*FileDescribe, error) {
	pathInfoPath := fmt.Sprintf("%s/%s/paths-info_post.json", pathInfoShaDir, fileName)
	fileDescribe := &FileDescribe{
		Name: fileName,
	}
	if exist := util.FileExists(pathInfoPath); exist {
		fileDescribe.IsDir = false
		cacheContent, err := m.fileDao.ReadCacheRequest(pathInfoPath)
		if err != nil {
			log.Errorf(fmt.Sprintf("read file:%s err", pathInfoPath))
			return nil, err
		}
		remoteRespPathsInfos := make([]common.PathsInfo, 0)
		err = sonic.Unmarshal(cacheContent.OriginContent, &remoteRespPathsInfos)
		if err != nil {
			log.Errorf("remoteRespPathsInfos Unmarshal err.%v", err)
			return nil, err
		}
		if filePath != "" {
			fileName = fmt.Sprintf("%s/%s", filePath, fileName)
		}
		for _, item := range remoteRespPathsInfos {
			if item.Path == fileName {
				fileDescribe.Size = item.Size
				break
			}
		}
	} else {
		fileDescribe.IsDir = true
	}
	return fileDescribe, nil
}

type FileDescribe struct {
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	IsDir bool   `json:"isDir"`
	Link  string `json:"link"`
}
