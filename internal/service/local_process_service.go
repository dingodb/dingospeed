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

package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"dingospeed/internal/data"
	"dingospeed/pkg/config"
	"dingospeed/pkg/consts"

	"github.com/bytedance/sonic"
	"go.uber.org/zap"
)

type LocalProcessService struct {
	Ctx context.Context
}

func NewLocalProcessService() *LocalProcessService {
	return &LocalProcessService{}
}

func (l *LocalProcessService) Run() {
	if config.SysConfig.GetOriginSchedulerModel() == consts.SchedulerModeCluster {
		go l.writeLocalFile()
		go l.startFileProcessSync()
	}
}

func (l *LocalProcessService) writeLocalFile() {
	for {
		select {
		case processParam, ok := <-data.GetLocalProcessChan():
			if !ok {
				return
			}
			marshal, err := sonic.Marshal(processParam)
			if err != nil {
				zap.S().Errorf("writeToDailyFile err.%v", err)
				continue
			}
			err = writeToDailyFile(processParam.OrgRepo, string(marshal))
			if err != nil {
				zap.S().Errorf("writeToDailyFile err.%v", err)
				continue
			}
		case <-l.Ctx.Done():
			zap.S().Warnf("writeLocalFile stop.")
			return
		}
	}
}

func writeToDailyFile(fileSuffix, data string) error {
	now := time.Now()
	dateStr := now.Format("2006010201")
	filename := fmt.Sprintf("%s-%s.log", dateStr, fileSuffix)
	processDir := filepath.Join(config.SysConfig.Server.Repos, "daily_process")
	if err := os.MkdirAll(processDir, 0755); err != nil {
		return fmt.Errorf("创建目录失败: %v", err)
	}
	filePath := filepath.Join(processDir, filename)
	file, err := os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("打开文件失败: %v", err)
	}
	defer file.Close()
	timestamp := now.Format("15:04:05")
	content := fmt.Sprintf("[%s] %s\n", timestamp, data)
	if _, err := file.WriteString(content); err != nil {
		return fmt.Errorf("写入文件失败: %v", err)
	}
	return nil
}

func (l *LocalProcessService) startFileProcessSync() {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
		case <-l.Ctx.Done():
			return
		}
	}
}

func (l *LocalProcessService) FileProcessSync() {

}

// 从复杂文件名中提取时间前缀（如 "2006010201-Qwen/Qwen-3B.log" → "2006010201"）
func extractTimePrefix(filename string) (string, error) {
	// 截取前10位作为时间候选（YYYYMMDDHH 共10位）
	if len(filename) < 10 {
		return "", fmt.Errorf("文件名过短，无法提取时间: %s", filename)
	}
	timePrefix := filename[:10]

	// 简单校验：前10位是否为数字（避免非时间开头的文件）
	for _, c := range timePrefix {
		if c < '0' || c > '9' {
			return "", fmt.Errorf("时间前缀包含非数字字符: %s", timePrefix)
		}
	}
	return timePrefix, nil
}

// 解析文件名中的时间（从提取的前缀转换为time.Time）
func parseFileTime(filename string) (time.Time, error) {
	timePrefix, err := extractTimePrefix(filename)
	if err != nil {
		return time.Time{}, err
	}
	// 解析为时间（格式：2006010215 对应 YYYYMMDDHH）
	return time.Parse("2006010215", timePrefix)
}

// 获取至少前1小时的文件（适配 2006010201-xxx/xxx.log 格式）
func listFilesBeforeLastHour(dirPath string) ([]string, error) {
	// 时间阈值：当前时间减1小时
	threshold := time.Now().Add(-1 * time.Hour)
	var result []string

	// 遍历目录中的所有文件（包括子目录，若有）
	err := filepath.WalkDir(dirPath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("访问路径失败: %w", err)
		}
		if d.IsDir() { // 跳过目录，只处理文件
			return nil
		}

		// 获取文件名（不含路径，如 "2006010201-Qwen/Qwen-3B.log"）
		filename := d.Name()

		// 解析文件时间
		fileTime, err := parseFileTime(filename)
		if err != nil {
			// 忽略格式不符合的文件（非目标命名规则）
			return nil
		}

		// 筛选：文件时间 ≤ 阈值（至少前1小时）
		if !fileTime.After(threshold) {
			result = append(result, path) // 记录完整路径
		}
		return nil
	})

	if err != nil {
		return nil, err
	}
	return result, nil
}
