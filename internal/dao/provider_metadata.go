package dao

import (
	"bytes"
	"encoding/json"
	"path/filepath"

	"dingospeed/pkg/common"
	"dingospeed/pkg/config"
	"dingospeed/pkg/repository"
	"dingospeed/pkg/util"
)

// CacheProviderFileInfo uses the HF paths-info envelope. Immutable file metadata
// need not be replaced on every read. The short lock also spans path inspection,
// avoiding concurrent Stat/rename conflicts on Windows across provider handlers.
func (f *FileDao) CacheProviderFileInfo(key repository.RepoKey, commit, requestedRevision string, info *common.PathsInfo) error {
	target := filepath.Join(key.PathsInfo(config.SysConfig.Repos(), commit, info.Path), "paths-info_post.json")
	body, err := json.Marshal([]*common.PathsInfo{info})
	if err != nil {
		return err
	}
	return f.CacheProviderResponse(target, commit, requestedRevision, body)
}

func (f *FileDao) CacheProviderResponse(target, commit, requestedRevision string, body []byte) error {
	fileReferenceLocks.Lock(target)
	defer fileReferenceLocks.Unlock(target)
	if err := repository.SafePath(config.SysConfig.Repos(), target); err != nil {
		return err
	}
	if cached, err := f.ReadCacheRequest(target); err == nil && bytes.Equal(cached.OriginContent, body) {
		return nil
	}
	if err := util.MakeDirs(target); err != nil {
		return err
	}
	return f.WriteCacheRequest(target, 200, map[string]string{"x-repo-commit": commit, "x-requested-revision": requestedRevision}, body)
}
