package data

import (
	"dingospeed/pkg/config"

	"github.com/patrickmn/go-cache"
)

type Cache interface {
	Set(key string, value interface{})
	Get(key string) (interface{}, bool)
	Delete(key string)
}

type GoCache struct {
	GCache *cache.Cache
}

func (f *GoCache) Set(key string, value interface{}) {
	f.GCache.Set(key, value, config.SysConfig.GetPrefetchBlockTTL())
}

func (f *GoCache) Get(key string) (interface{}, bool) {
	return f.GCache.Get(key)
}

func (f *GoCache) Delete(key string) {
	f.GCache.Delete(key)
}
