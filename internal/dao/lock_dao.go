package dao

import (
	"fmt"
	"sync"
	"time"

	"dingospeed/internal/data"
)

type LockDao struct {
	baseData        *data.BaseData
	metaFileMu      sync.Mutex
	metaReqMu       sync.Mutex
	metaFileTimeout time.Duration
}

func NewLockDao(baseData *data.BaseData) *LockDao {
	return &LockDao{baseData: baseData, metaFileTimeout: 30 * time.Second}
}

// api meta file lock，for read and write metafile
func (f *LockDao) getMetaFileLock(apiPath string) *sync.RWMutex {
	if val, ok := f.baseData.Cache.Get(apiPath); ok {
		f.baseData.Cache.Set(apiPath, val, f.metaFileTimeout)
		return val.(*sync.RWMutex)
	}
	f.metaFileMu.Lock()
	defer f.metaFileMu.Unlock()
	if val, ok := f.baseData.Cache.Get(apiPath); ok {
		f.baseData.Cache.Set(apiPath, val, f.metaFileTimeout)
		return val.(*sync.RWMutex)
	}
	newLock := &sync.RWMutex{}
	f.baseData.Cache.Set(apiPath, newLock, f.metaFileTimeout)
	return newLock
}

func (f *LockDao) getMetaDataReqLock(orgRepoKey string) *sync.RWMutex {
	if val, ok := f.baseData.Cache.Get(orgRepoKey); ok {
		f.baseData.Cache.Set(orgRepoKey, val, f.metaFileTimeout)
		return val.(*sync.RWMutex)
	}
	f.metaReqMu.Lock()
	defer f.metaReqMu.Unlock()
	if val, ok := f.baseData.Cache.Get(orgRepoKey); ok {
		f.baseData.Cache.Set(orgRepoKey, val, f.metaFileTimeout)
		return val.(*sync.RWMutex)
	}
	newLock := &sync.RWMutex{}
	f.baseData.Cache.Set(orgRepoKey, newLock, f.metaFileTimeout)
	return newLock
}

func GetMetaShaRepoKey(repo, commit, authorization string) string {
	return fmt.Sprintf("meta/%s/%s/%s", repo, commit, authorization)
}

func GetMetaDataReqKey(repoType, orgRepo, commit string) string {
	return fmt.Sprintf("metadatareq/%s/%s/%s", repoType, orgRepo, commit)
}

func GetFilePathInfoKey(repoType, orgRepo, authorization string) string {
	return fmt.Sprintf("filePathInfo/%s/%s/%s", repoType, orgRepo, authorization)
}
