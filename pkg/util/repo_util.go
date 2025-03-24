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

package util

import (
	"encoding/gob"
	"fmt"
	"os"

	"dingo-hfmirror/pkg/common"
)

func GetOrgRepo(org, repo string) string {
	if org == "" {
		return repo
	} else {
		return fmt.Sprintf("%s/%s", org, repo)
	}
}

// MakeDirs 确保指定路径对应的目录存在
func MakeDirs(path string) error {
	// 检查目录是否存在
	_, err := os.Stat(path)
	if os.IsNotExist(err) {
		// 目录不存在，创建目录
		err = os.MkdirAll(path, 0755)
		if err != nil {
			return err
		}
	} else if err != nil {
		// 其他错误
		return err
	}
	return nil
}

// IsDir 判断所给路径是否为文件夹
func IsDir(path string) bool {
	s, err := os.Stat(path)
	if err != nil {
		return false
	}
	return s.IsDir()
}

// IsFile 判断所给文件是否存在
func IsFile(path string) bool {
	s, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !s.IsDir()
}

// GetFileSize 获取文件大小
func GetFileSize(path string) int64 {
	fh, err := os.Stat(path)
	if err != nil {
		fmt.Printf("读取文件%s失败, err: %s\n", path, err)
	}
	return fh.Size()
}

// StoreMetadata 保存文件元数据
func StoreMetadata(filePath string, metadata *common.FileMetadata) error {
	// 写入文件
	file, err := os.OpenFile(filePath, os.O_WRONLY|os.O_CREATE, 0666)
	if err != nil {
		fmt.Printf("写元数据文件%s失败\n", filePath)
		return err
	}
	defer file.Close()

	enc := gob.NewEncoder(file)
	err = enc.Encode(metadata)
	if err != nil {
		fmt.Printf("写元数据文件%s失败\n", filePath)
		return err
	}
	return nil
}

func SplitFileToSegment(fileSize int64, blockSize int64) (int, []*common.Segment) {
	segments := make([]*common.Segment, 0)
	start, index := int64(0), 0
	for start < fileSize {
		end := start + blockSize
		if end > fileSize {
			end = fileSize
		}
		segments = append(segments, &common.Segment{Index: index, Start: start, End: end})
		end++
		index++
		start = end
	}
	return index, segments
}
