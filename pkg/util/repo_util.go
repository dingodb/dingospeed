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
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"dingo-hfmirror/pkg/common"

	"github.com/bytedance/sonic"
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
	fileInfo, err := os.Stat(path)
	if err == nil {
		if fileInfo.IsDir() {
			// 如果路径本身就是目录，直接使用该路径
			return nil
		}
	}

	// 如果路径不是目录，获取其父目录
	saveDir := filepath.Dir(path)
	// 检查目录是否存在
	_, err = os.Stat(saveDir)
	if os.IsNotExist(err) {
		// 目录不存在，创建目录
		err = os.MkdirAll(saveDir, 0755)
		if err != nil {
			return err
		}
	} else if err != nil {
		// 其他错误
		return err
	}
	return nil
}

// FileExists 函数用于判断文件是否存在
func FileExists(filename string) bool {
	_, err := os.Stat(filename)
	if os.IsNotExist(err) {
		return false
	}
	return err == nil
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

func ReadFileToBytes(filename string) ([]byte, error) {
	return os.ReadFile(filename)
}

func WriteDataToFile(filename string, data interface{}) error {
	jsonData, err := sonic.Marshal(data)
	if err != nil {
		return fmt.Errorf("JSON 编码出错: %w", err)
	}
	file, err := os.OpenFile(filename, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("打开文件出错: %w", err)
	}
	defer file.Close()
	_, err = file.Write(jsonData)
	if err != nil {
		return fmt.Errorf("写入文件出错: %w", err)
	}
	return nil
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

func GetFolderSize(path string) (int64, error) {
	var size int64
	err := filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			size += info.Size()
		}
		return nil
	})
	return size, err
}

// FileWithPath 自定义结构体，用于存储文件信息和对应的路径
type FileWithPath struct {
	Info os.FileInfo
	Path string
}

// getAccessTime 跨平台获取文件访问时间
func getAccessTime(info os.FileInfo) time.Time {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		if ts, ok := tryGetAtime(stat); ok {
			return time.Unix(ts.Sec, ts.Nsec)
		}
	}
	// 若无法获取访问时间，使用修改时间替代
	return info.ModTime()
}

// tryGetAtime 尝试不同方式获取文件访问时间
func tryGetAtime(stat *syscall.Stat_t) (syscall.Timespec, bool) {
	if v, ok := interface{}(stat).(interface{ Atimespec() syscall.Timespec }); ok {
		return v.Atimespec(), true
	}
	if v, ok := interface{}(stat).(interface{ Atim() syscall.Timespec }); ok {
		return v.Atim(), true
	}

	return syscall.Timespec{}, false
}

// SortFilesByAccessTime 按文件访问时间对指定路径下的文件进行正序排序
func SortFilesByAccessTime(path string) ([]FileWithPath, error) {
	var filesWithPaths []FileWithPath
	err := filepath.Walk(path, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			filesWithPaths = append(filesWithPaths, FileWithPath{
				Info: info,
				Path: p,
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// 按访问时间对文件进行正序排序，秒数相同则比较纳秒
	sort.Slice(filesWithPaths, func(i, j int) bool {
		timeI := getAccessTime(filesWithPaths[i].Info)
		timeJ := getAccessTime(filesWithPaths[j].Info)
		if timeI.Unix() == timeJ.Unix() {
			return timeI.Nanosecond() < timeJ.Nanosecond()
		}
		return timeI.Before(timeJ)
	})

	return filesWithPaths, nil
}

func SortFilesByModifyTime(path string) ([]FileWithPath, error) {
	filesWithPaths, err := SortFilesByAccessTime(path)
	return filesWithPaths, err
}

// SortFilesBySize 按文件大小对指定路径下的文件进行降序排序
func SortFilesBySize(path string) ([]FileWithPath, error) {
	var filesWithPaths []FileWithPath
	// 获取今天的日期
	now := time.Now()
	year, month, day := now.Date()
	today := time.Date(year, month, day, 0, 0, 0, 0, now.Location())

	err := filepath.Walk(path, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			// 获取文件修改时间
			modTime := info.ModTime()
			// 检查文件修改时间是否不是今天
			if modTime.Before(today) {
				filesWithPaths = append(filesWithPaths, FileWithPath{
					Info: info,
					Path: p,
				})
			}
		}
		return nil
	})

	if err != nil {
		return nil, err
	}

	sort.Slice(filesWithPaths, func(i, j int) bool {
		// 比较文件大小，降序排序
		return filesWithPaths[i].Info.Size() > filesWithPaths[j].Info.Size()
	})

	return filesWithPaths, nil
}

func ConvertBytesToHumanReadable(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(bytes)/float64(div), "KMGTPE"[exp])
}
