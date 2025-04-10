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
