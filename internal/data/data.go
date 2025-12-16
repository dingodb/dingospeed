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

package data

import (
	"dingospeed/pkg/config"

	"github.com/google/wire"
	"github.com/patrickmn/go-cache"
)

var BaseDataProvider = wire.NewSet(NewBaseData)

// 缓存预读取的文件块，默认每个文件16个块

type BaseData struct {
	Cache *cache.Cache
}

func NewBaseData() *BaseData {
	gCache := cache.New(config.SysConfig.GetDefaultExpiration(), config.SysConfig.GetCleanupInterval())
	initGlobal(gCache)
	return &BaseData{
		Cache: gCache,
	}
}

func initGlobal(gCache *cache.Cache) {
	if FileBlockCache == nil {
		// 默认使用ristretto
		FileBlockCache = &GoCache{
			GCache: gCache,
		}
	}
	if config.SysConfig.IsCluster() {
		fileProcessChan = make(chan *FileProcessParam, 100)
		localOperationChan = make(chan *LocalOperation, 100)
	}
}
