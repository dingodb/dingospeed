package downloader

import (
	"sync"
	"sync/atomic"

	"dingospeed/pkg/common"
	"dingospeed/pkg/config"

	"go.uber.org/zap"
)

var (
	instance *DingCacheManager
	once     sync.Once
)

func GetInstance() *DingCacheManager {
	once.Do(func() {
		instance = &DingCacheManager{
			dingCacheMap: common.NewSafeMap[string, *DingCache](),
			dingCacheRef: common.NewSafeMap[string, *atomic.Int64](),
		}
	})
	return instance
}

type DingCacheManager struct {
	dingCacheMap *common.SafeMap[string, *DingCache]
	dingCacheRef *common.SafeMap[string, *atomic.Int64]
	mu           sync.RWMutex
}

func (f *DingCacheManager) GetDingFile(savePath string, fileSize int64) (*DingCache, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var (
		dingFile *DingCache
		ok       bool
		err      error
	)
	if dingFile, ok = f.dingCacheMap.Get(savePath); ok {
		if refCount, ok := f.dingCacheRef.Get(savePath); ok {
			refCount.Add(1)
		} else {
			zap.S().Errorf("dingCacheRef key is not exist.key:%s", savePath)
		}
		return dingFile, nil
	} else {
		if dingFile, err = NewDingCache(savePath, config.SysConfig.Download.BlockSize); err != nil {
			zap.S().Errorf("NewDingCache err.%v", err)
			return nil, err
		}
		if dingFile.GetFileSize() == 0 && fileSize > 0 { // 表示首次获取当前文件句柄，需要Resize。
			if err = dingFile.Resize(fileSize); err != nil {
				zap.S().Errorf("Resize err.%v", err)
				return nil, err
			}
		}

		f.dingCacheMap.Set(savePath, dingFile)
		var counter atomic.Int64
		counter.Store(1)
		f.dingCacheRef.Set(savePath, &counter)
	}
	return dingFile, nil
}

// IsInUse 报告进程内是否还有人持有这个文件的句柄。
//
// 缓存管理的彻底删除要用它挡住“正在下载/上传的文件被 os.Remove 掉”：
// Linux 上 unlink 之后句柄仍然有效，写入会全部落到孤儿 inode 上，
// 位图更新丢失且没有任何报错，表现为一个明明下载完了却读不出内容的文件。
func (f *DingCacheManager) IsInUse(savePath string) bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	refCount, ok := f.dingCacheRef.Get(savePath)
	return ok && refCount.Load() > 0
}

func (f *DingCacheManager) ReleasedDingFile(savePath string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	refCount, ok := f.dingCacheRef.Get(savePath)
	if !ok {
		return
	}
	refCount.Add(-1)
	zap.S().Debugf("ReleasedDingFile:%s, refcount:%d", savePath, refCount.Load())
	if refCount.Load() <= 0 {
		if dingFile, ok := f.dingCacheMap.Get(savePath); ok {
			dingFile.Close()
		}
		f.dingCacheMap.Delete(savePath)
		f.dingCacheRef.Delete(savePath)
	} else {
		f.dingCacheRef.Set(savePath, refCount)
	}
}
