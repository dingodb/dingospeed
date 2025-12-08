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
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"dingospeed/internal/dao"
	"dingospeed/internal/data"
	"dingospeed/pkg/config"
	"dingospeed/pkg/consts"
	"dingospeed/pkg/proto/manager"
	"dingospeed/pkg/util"

	"github.com/bytedance/sonic"
	"github.com/robfig/cron/v3"
	"go.uber.org/zap"
)

type LocalProcessService struct {
	Ctx          context.Context
	schedulerDao *dao.SchedulerDao
}

func NewLocalProcessService(schedulerDao *dao.SchedulerDao) *LocalProcessService {
	return &LocalProcessService{schedulerDao: schedulerDao}
}

func (l *LocalProcessService) Run() {
	if config.SysConfig.GetOriginSchedulerModel() == consts.SchedulerModeCluster {
		go l.writeLocalFile()
		if config.SysConfig.Online() {
			go l.startFileProcessSync()
		}
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
			err = writeToDailyFile(processParam.Repo, string(marshal))
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

func (l *LocalProcessService) startFileProcessSync() {
	c := cron.New(cron.WithSeconds())
	_, err := c.AddFunc("0 0 * * * ?", func() {
		err := l.FileProcessSync()
		if err != nil {
			zap.S().Errorf("FileProcessSync err.%v", err)
		}
	})
	if err != nil {
		zap.S().Errorf("添加PersistRepo任务失败: %v", err)
		return
	}
	c.Start()
	defer c.Stop()
	select {
	case <-l.Ctx.Done():
		return
	}
}

func (l *LocalProcessService) FileProcessSync() error {
	processDir := filepath.Join(config.SysConfig.Server.Repos, "daily_process")
	processFilePaths, err := listFilesBeforeLastHour(processDir)
	if err != nil {
		return err
	}
	for _, filePath := range processFilePaths {
		err = l.readAndSyncFileProcess(filePath)
		if err != nil {
			zap.S().Errorf("readAndSyncFileProcess err.file:%s, %v", filePath, err)
			continue
		}
		err = util.DeleteFile(filePath)
		if err != nil {
			zap.S().Errorf("DeleteFile err.file:%s, %v", filePath, err)
			continue
		}
	}
	return nil
}

func (l *LocalProcessService) readAndSyncFileProcess(filePath string) error {
	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("打开文件失败: %w", err)
	}
	defer file.Close()
	var processParams []*data.FileProcessParam
	scanner := bufio.NewScanner(file)
	count, batchSize := 0, 10
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		jsonStr, err := extractJSON(line)
		if err != nil {
			fmt.Printf("跳过无效行: %v\n", err)
			return err
		}
		var param *data.FileProcessParam
		if err = sonic.Unmarshal([]byte(jsonStr), &param); err != nil {
			fmt.Printf("解析 JSON 失败: %v, 内容: %s\n", err, jsonStr)
			return err
		}
		processParams = append(processParams, param)
		count++
		if count == batchSize {
			err = l.SyncFileProcess(processParams)
			if err != nil {
				return err
			}
		}
	}
	if count > 0 {
		err = l.SyncFileProcess(processParams)
		if err != nil {
			return err
		}
	}
	if err = scanner.Err(); err != nil {
		return fmt.Errorf("读取文件错误: %w", err)
	}
	return nil
}

func (l *LocalProcessService) SyncFileProcess(processParams []*data.FileProcessParam) error {
	fileProcessEntries := make([]*manager.FileProcessEntry, 0)
	for _, param := range processParams {
		fileProcessEntries = append(fileProcessEntries, &manager.FileProcessEntry{
			DataType:   param.Datatype,
			Org:        param.Org,
			Repo:       param.Repo,
			Name:       param.Name,
			Etag:       param.Etag,
			InstanceId: config.SysConfig.Scheduler.Discovery.InstanceId,
			StartPos:   param.StartPos,
			EndPos:     param.EndPos,
			FileSize:   param.FileSize,
			Status:     param.Status,
			ProcessId:  param.ProcessId,
		})
	}
	err := l.schedulerDao.SyncFileProcess(&manager.SyncFileProcessReq{
		FileProcessEntries: fileProcessEntries,
	})
	if err != nil {
		zap.S().Errorf("SyncFileProcess err.%v", err)
		return err
	}
	return nil
}

func extractJSON(logLine string) (string, error) {
	sepIndex := strings.Index(logLine, "] ")
	if sepIndex == -1 {
		return "", fmt.Errorf("日志格式错误，未找到时间戳分隔符: %s", logLine)
	}
	jsonStr := logLine[sepIndex+2:]
	return jsonStr, nil
}

func extractTimePrefix(filename string) (string, error) {
	if len(filename) < 10 {
		return "", fmt.Errorf("文件名过短，无法提取时间: %s", filename)
	}
	timePrefix := filename[:10]
	for _, c := range timePrefix {
		if c < '0' || c > '9' {
			return "", fmt.Errorf("时间前缀包含非数字字符: %s", timePrefix)
		}
	}
	return timePrefix, nil
}

func parseFileTime(filename string) (time.Time, error) {
	timePrefix, err := extractTimePrefix(filename)
	if err != nil {
		return time.Time{}, err
	}
	return time.Parse("2006010215", timePrefix)
}

// 获取至少前1小时的文件（适配 2006010201-xxx/xxx.log 格式）
func listFilesBeforeLastHour(dirPath string) ([]string, error) {
	threshold := time.Now().Add(-1 * time.Hour)
	var result []string
	if !util.FileExists(dirPath) {
		return result, nil
	}
	err := filepath.WalkDir(dirPath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("访问路径失败: %w", err)
		}
		if d.IsDir() { // 跳过目录，只处理文件
			return nil
		}
		filename := d.Name()
		fileTime, err := parseFileTime(filename)
		if err != nil {
			// 忽略格式不符合的文件（非目标命名规则）
			return nil
		}
		if fileTime.Before(threshold) {
			result = append(result, path) // 记录完整路径
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
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
