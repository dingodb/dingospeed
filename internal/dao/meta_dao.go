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
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"dingospeed/internal/data"
	"dingospeed/pkg/common"
	"dingospeed/pkg/config"
	"dingospeed/pkg/dependency"
	myerr "dingospeed/pkg/error"
	"dingospeed/pkg/repository"
	"dingospeed/pkg/util"

	"github.com/bytedance/sonic"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

type MetaDao struct {
	fileDao  *FileDao
	lockDao  *LockDao
	baseData *data.BaseData
}

func NewMetaDao(fileDao *FileDao, lockDao *LockDao, baseData *data.BaseData) *MetaDao {
	return &MetaDao{
		fileDao:  fileDao,
		lockDao:  lockDao,
		baseData: baseData,
	}
}

func (m *MetaDao) WhoamiV2Generator(c echo.Context) error {
	newHeaders := make(map[string]string, 0)
	for k := range c.Request().Header {
		v := c.Request().Header.Get(k)
		lowerKey := strings.ToLower(k)
		if lowerKey == "host" {
			continue
		}
		newHeaders[lowerKey] = v
	}
	resp, err := util.Get("/api/whoami-v2", newHeaders)
	if err != nil {
		zap.S().Errorf("WhoamiV2Generator err.%v", err)
		return err
	}
	extractHeaders := resp.ExtractHeaders(resp.Headers)
	for k, vv := range extractHeaders {
		c.Response().Header().Add(k, vv)
	}
	c.Response().WriteHeader(resp.StatusCode)
	if _, err := c.Response().Write(resp.Body); err != nil {
		zap.S().Errorf("响应内容回传失败.%v", err)
	}
	return nil
}

func (m *MetaDao) ReposGenerator(c echo.Context) error {
	all, err := repository.List(config.SysConfig.Repos())
	if err != nil {
		return echo.NewHTTPError(500, "repository registry unavailable")
	}
	groups := map[string]interface{}{"datasets_repos": []string{}, "models_repos": []string{}, "spaces_repos": []string{}}
	for _, d := range all {
		name := d.RepoType + "_repos"
		groups[name] = append(groups[name].([]string), d.ID())
	}
	return c.Render(http.StatusOK, "repos.html", groups)
}

func (m *MetaDao) RepoRefs(repoType string, orgRepo string, authorization string) (*common.Response, error) {
	upstream, err := UpstreamRepo(repoType, orgRepo)
	if err != nil {
		return nil, err
	}
	refsUri := fmt.Sprintf("/api/%s/%s/refs", repoType, repository.EscapeURLPath(upstream))
	headers := map[string]string{}
	if authorization != "" {
		headers["authorization"] = authorization
	}
	resp, err := util.RetryRequest(func() (*common.Response, error) {
		return util.Get(refsUri, headers)
	})
	return resp, err
}

func (m *MetaDao) ForwardRefs(originalReq echo.Context) (*http.Response, error) {
	return util.ForwardRequest(originalReq)
}

// RefreshPreheatMetadata resolves a moving HF branch once for an explicit task.
// Ordinary catalog browsing never calls this method.
func (m *MetaDao) RefreshPreheatMetadata(k repository.RepoKey, revision, authorization string) (*CommitHfSha, error) {
	return m.RefreshPreheatMetadataContext(context.Background(), k, revision, authorization)
}
func (m *MetaDao) RefreshPreheatMetadataContext(ctx context.Context, k repository.RepoKey, revision, authorization string) (*CommitHfSha, error) {
	if err := k.Validate(); err != nil {
		return nil, err
	}
	if k.Namespace != repository.HuggingFace || !config.SysConfig.Online() {
		return nil, fmt.Errorf("HF prewarm requires an online Hugging Face source")
	}
	if err := repository.Segment(revision); err != nil {
		return nil, err
	}
	lock := m.lockDao.getMetaDataReqLock(GetMetaDataReqKey(k.RepoType, k.ID(), revision))
	lock.Lock()
	defer lock.Unlock()
	resp, err := m.fileDao.RemoteRequestMetaContext(ctx, "get", k.RepoType, k.ID(), revision, authorization)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, myerr.NewAppendCode(resp.StatusCode, "HF prewarm metadata request failed")
	}
	var meta CommitHfSha
	if err := sonic.Unmarshal(resp.Body, &meta); err != nil {
		return nil, err
	}
	if err := repository.Segment(meta.Sha); err != nil {
		return nil, err
	}
	for _, file := range meta.Siblings {
		if err := repository.Relative(file.Rfilename); err != nil {
			return nil, err
		}
	}
	headers := resp.ExtractHeaders(resp.Headers)
	if err := m.writeApiMetaFile(k.RepoType, k.ID(), meta.Sha, "get", resp.StatusCode, headers, resp.Body); err != nil {
		return nil, err
	}
	if err := m.writeApiMetaFile(k.RepoType, k.ID(), revision, "get", resp.StatusCode, headers, resp.Body); err != nil {
		return nil, err
	}
	return &meta, nil
}

func (m *MetaDao) GetMetadata(repoType, orgRepo, revision, method, authorization string) (*common.CacheContent, error) {
	var (
		cacheContent *common.CacheContent
		err          error
	)
	orgRepoKey := GetMetaDataReqKey(repoType, orgRepo, revision)
	lock := m.lockDao.getMetaDataReqLock(orgRepoKey)
	lock.Lock()
	defer lock.Unlock()
	commitSha, err := m.fileDao.GetFileCommitSha(repoType, orgRepo, revision, authorization, "meta")
	if err != nil {
		return nil, err
	}
	apiDir := RepositoryKey(repoType, orgRepo).Revision(config.SysConfig.Repos(), commitSha)
	apiMetaPath := filepath.Join(apiDir, "meta_"+method+".json")
	if config.SysConfig.Online() && !IsLocalOrgRepo(orgRepo) {
		if util.FileExists(apiMetaPath) {
			if cacheContent, err = m.fileDao.ReadCacheRequest(apiMetaPath); err != nil {
				zap.S().Errorf("ReadCacheRequest err.%v", err)
				if cacheContent, err = m.requestAndSaveMeta(repoType, orgRepo, revision, commitSha, method, authorization); err != nil {
					return nil, err
				}
			}
		} else {
			if cacheContent, err = m.requestAndSaveMeta(repoType, orgRepo, revision, commitSha, method, authorization); err != nil {
				return nil, err
			}
		}
	} else {
		exists, accessErr := util.PathExists(apiMetaPath)
		if accessErr != nil {
			util.ObserveFileAccessFailure("stat", accessErr)
			return nil, localMetadataError(accessErr)
		}
		if exists {
			if cacheContent, err = m.fileDao.ReadCacheRequest(apiMetaPath); err != nil {
				util.ObserveFileAccessFailure("read", err)
				zap.S().Errorf("ReadCacheRequest err.%v", err)
				if errors.Is(err, os.ErrNotExist) {
					return nil, myerr.NewAppendCode(http.StatusNotFound, fmt.Sprintf("%s not exist", orgRepo))
				}
				return nil, localMetadataError(err)
			}
			dependency.Default.Observe(dependency.MetadataRead, true, time.Now())
		} else {
			return nil, myerr.NewAppendCode(http.StatusNotFound, fmt.Sprintf("%s not exist", orgRepo))
		}
	}
	cacheContent, err = m.ensureLocalMetadataID(orgRepo, cacheContent)
	if err != nil {
		return nil, err
	}
	if !IsLocalOrgRepo(orgRepo) {
		k, err := repository.ParseID(repoType, orgRepo)
		if err != nil {
			return nil, err
		}
		if err = repository.Register(config.SysConfig.Repos(), repository.Remote(k)); err != nil {
			// Recording a remote cache descriptor is not required to deliver an
			// online response. Validation/conflict errors still fail closed.
			if !config.SysConfig.Online() || !metadataCacheIOError(err) {
				return nil, err
			}
		}
	}
	return cacheContent, nil
}

func (m *MetaDao) ensureLocalMetadataID(orgRepo string, cacheContent *common.CacheContent) (*common.CacheContent, error) {
	if cacheContent == nil || !IsLocalOrgRepo(orgRepo) || len(cacheContent.OriginContent) == 0 {
		return cacheContent, nil
	}
	var metadata map[string]interface{}
	if err := sonic.Unmarshal(cacheContent.OriginContent, &metadata); err != nil {
		return nil, err
	}
	if id, ok := metadata["id"].(string); ok && id != "" {
		return cacheContent, nil
	}
	metadata["id"] = orgRepo
	body, err := sonic.Marshal(metadata)
	if err != nil {
		return nil, err
	}
	headers := make(map[string]string, len(cacheContent.Headers)+1)
	for k, v := range cacheContent.Headers {
		headers[k] = v
	}
	headers["content-length"] = fmt.Sprintf("%d", len(body))
	cacheContent.Headers = headers
	cacheContent.OriginContent = body
	return cacheContent, nil
}

func (m *MetaDao) requestAndSaveMeta(repoType, orgRepo, revision, commitSha, method, authorization string) (*common.CacheContent, error) {
	resp, err := m.fileDao.RemoteRequestMeta(method, repoType, orgRepo, revision, authorization)
	if err != nil {
		zap.S().Errorf("requestAndSaveMeta %s err.%v", method, err)
		return nil, err
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusTemporaryRedirect {
		return nil, myerr.NewAppendCode(resp.StatusCode, "request err")
	}
	extractHeaders := resp.ExtractHeaders(resp.Headers)
	content := &common.CacheContent{StatusCode: resp.StatusCode, Headers: extractHeaders, OriginContent: resp.Body}
	// This helper is used only for online metadata delivery. Keep failures in
	// writeApiMetaFile visible to durable writers; tolerate filesystem cache
	// failures here only after a complete successful upstream response exists.
	cacheResult := func(err error) (*common.CacheContent, error) {
		if metadataCacheIOError(err) {
			return content, nil
		}
		return nil, err
	}
	mainVersion := "main"
	if revision == mainVersion {
		err = m.writeApiMetaFile(repoType, orgRepo, revision, method, resp.StatusCode, extractHeaders, resp.Body)
		if err != nil {
			return cacheResult(err)
		}
	} else {
		apiDir := RepositoryKey(repoType, orgRepo).Revision(config.SysConfig.Repos(), mainVersion)
		apiMetaPath := filepath.Join(apiDir, "meta_"+method+".json")
		if !util.FileExists(apiMetaPath) {
			err = m.writeApiMetaFile(repoType, orgRepo, mainVersion, method, resp.StatusCode, extractHeaders, resp.Body) // create main dir
			if err != nil {
				return cacheResult(err)
			}
		}
	}

	err = m.writeApiMetaFile(repoType, orgRepo, commitSha, method, resp.StatusCode, extractHeaders, resp.Body)
	if err != nil {
		return cacheResult(err)
	}
	return content, nil
}

// Only for cache persistence operations: ENOENT during a write is a failed
// cache write too. flock may return a bare errno rather than an os.PathError.
// Validation, namespace conflicts and lock contention are not I/O errors.
func metadataCacheIOError(err error) bool {
	var pathErr *os.PathError
	var linkErr *os.LinkError
	var errno syscall.Errno
	return errors.As(err, &pathErr) || errors.As(err, &linkErr) || errors.As(err, &errno)
}

func (m *MetaDao) writeApiMetaFile(repoType, orgRepo, commitSha, method string, statusCode int, extractHeaders map[string]string, body []byte) error {
	apiDir := RepositoryKey(repoType, orgRepo).Revision(config.SysConfig.Repos(), commitSha)
	apiMetaPath := filepath.Join(apiDir, "meta_"+method+".json")
	err := util.MakeDirs(apiMetaPath)
	if err != nil {
		util.ObserveFileAccessFailure("mkdir", err)
		zap.S().Errorf("create %s dir err.%v", apiMetaPath, err)
		return err
	}
	if err = m.fileDao.WriteCacheRequest(apiMetaPath, statusCode, extractHeaders, body); err != nil {
		util.ObserveFileAccessFailure("write", err)
		zap.S().Errorf("writeCacheRequest err.%v", err)
		return err
	}
	dependency.Default.Observe(dependency.MetadataWrite, true, time.Now())
	return nil
}
