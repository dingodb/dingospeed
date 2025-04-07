package cache

import (
	"github.com/dgraph-io/ristretto/v2"
)

// 缓存预读取的文件块，默认每个文件16个块
var FileBlockCache *ristretto.Cache[string, []byte]

func InitCache() {
	if FileBlockCache == nil {
		cache, err := ristretto.NewCache(&ristretto.Config[string, []byte]{
			NumCounters: 1e7,     // 计数器数量，用于预估缓存项的使用频率
			MaxCost:     1 << 30, // 缓存的最大成本，这里设置为 1GB
			BufferItems: 64,      // 每个分片的缓冲区大小
		})
		if err != nil {
			panic(err)
		}
		FileBlockCache = cache
	}
}
