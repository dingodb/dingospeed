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

package common

import (
	"time"
)

// FileInfo 列出文件元信息
type FileInfo struct {
	Filename string // 文件名
	Filesize int64  // 文件大小
	Filetype string // 文件类型（目前有普通文件和切片文件两种）
}

// ListFileInfos 文件列表结构
type ListFileInfos struct {
	Files []FileInfo
}

type Segment struct {
	Index int
	Start int64
	End   int64
}

// FileMetadata 文件片元数据
type FileMetadata struct {
	Fid        string    // 操作文件ID，随机生成的UUID
	Filesize   int64     // 文件大小（字节单位）
	Filename   string    // 文件名称
	SliceNum   int       // 切片数量
	Md5sum     string    // 文件md5值
	ModifyTime time.Time // 文件修改时间
	Segments   []*Segment
}

type Response struct {
	StatusCode int
	Headers    map[string]interface{}
	Body       []byte
}

type PathsInfo struct {
	Type string `json:"type"`
	Oid  string `json:"oid"`
	Size int64  `json:"size"`
	Path string `json:"path"`
}
