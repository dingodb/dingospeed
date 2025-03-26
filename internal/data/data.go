package cache

import (
	"github.com/dgraph-io/ristretto/v2"
)

var FileBlockCache *ristretto.Cache[string, []byte]

func InitCache() {
	if FileBlockCache == nil {
		cache, err := ristretto.NewCache(&ristretto.Config[string, []byte]{
			NumCounters: 1e7,     // number of keys to track frequency of (10M).
			MaxCost:     1 << 30, // maximum cost of cache (1GB).
			BufferItems: 64,      // number of keys per Get buffer.
		})
		if err != nil {
			panic(err)
		}
		FileBlockCache = cache
	}
}
