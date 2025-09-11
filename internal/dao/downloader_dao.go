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

package dao

import (
	"context"
	"fmt"
	"sync"
	"time"

	"dingospeed/internal/downloader"
	"dingospeed/pkg/common"
	"dingospeed/pkg/config"
	"dingospeed/pkg/consts"
	"dingospeed/pkg/proto/manager"
	"dingospeed/pkg/util"

	"go.uber.org/zap"
)

type DownloaderDao struct {
	schedulerDao *SchedulerDao
}

func NewDownloaderDao(schedulerDao *SchedulerDao) *DownloaderDao {
	return &DownloaderDao{
		schedulerDao: schedulerDao,
	}
}

// 整个文件
func (d *DownloaderDao) FileDownload(startPos, endPos int64, isInnerRequest bool, taskParam *downloader.TaskParam) {
	var (
		wg sync.WaitGroup
	)
	defer close(taskParam.ResponseChan)
	dingCacheManager := downloader.GetInstance()
	dingFile, err := dingCacheManager.GetDingFile(taskParam.BlobsFile, taskParam.FileSize)
	if err != nil {
		zap.S().Errorf("GetDingFile err.%v", err)
		return
	}
	defer func() {
		dingCacheManager.ReleasedDingFile(taskParam.BlobsFile)
	}()
	taskParam.DingFile = dingFile
	tasks := d.constructTask(startPos, endPos, isInnerRequest, taskParam)
	wg.Add(1)
	go func() {
		defer func() {
			wg.Done()
		}()
		for i := 0; i < len(tasks); i++ {
			if taskParam.Context.Err() != nil {
				break
			}
			task := tasks[i]
			if i == 0 {
				task.GetResponseChan() <- []byte{} // 先建立长连接
			}
			task.OutResult()
		}
	}()
	if len(tasks) > 0 {
		wg.Add(1)
		go func() {
			defer func() {
				wg.Done()
			}()
			doTask(taskParam.Context, tasks)
		}()
	}
	wg.Wait() // 等待协程池所有远程下载任务执行完毕
}

func (d *DownloaderDao) constructTask(startPos, endPos int64, isInnerRequest bool, taskParam *downloader.TaskParam) []common.Task {
	var (
		tasks         []common.Task
		ctx           = taskParam.Context
		existPosition bool
		curPos        int64
	)
	if taskParam.FileSize <= config.SysConfig.GetMinimumFileSize() {
		goto localTask
	}
	existPosition, curPos = analysisFilePosition(taskParam.DingFile, startPos, endPos)
	// if config.SysConfig.IsCluster() && existPosition {
	// 	// 同步check本地仓库和数据库的偏移量，若数据库滞后，则需更新该数据。
	// 	if err := d.SyncFileProcess(taskParam.DataType, taskParam.OrgRepo, taskParam.FileName, taskParam.Etag, curPos, endPos, taskParam.FileSize); err != nil {
	// 		zap.S().Errorf("SyncFileProcess err.%v", err)
	// 	}
	// }
	if !isInnerRequest && config.SysConfig.IsCluster() && !existPosition {
		if response, err := d.getRequestDomainScheduler(taskParam.DataType, taskParam.OrgRepo, taskParam.FileName, taskParam.Etag, curPos, endPos, taskParam.FileSize); err != nil {
			zap.S().Errorf("getRequestDomainScheduler err.%v", err)
			goto localTask
		} else {
			ctx = context.WithValue(ctx, consts.KeyProcessId, response.ProcessId)
			ctx = context.WithValue(ctx, consts.KeyMasterInstanceId, response.MasterInstanceId)
			taskParam.Context = ctx
			if response.SchedulerType == consts.SchedulerYes {
				if curPos != 0 {
					tasks = getContiguousRanges(startPos, curPos, taskParam)
				}
				speedDomain := fmt.Sprintf("http://%s:%d", response.Host, response.Port) // 此刻向该节点发起远程下载请求
				if endPos <= response.MaxOffset {
					taskParam.Domain = speedDomain
					speedTasks := getContiguousRanges(curPos, endPos, taskParam)
					tasks = append(tasks, speedTasks...)
				} else {
					// 需要重新拆分任务
					taskParam.Domain = speedDomain
					beforeTasks := getContiguousRanges(curPos, response.MaxOffset, taskParam)
					taskParam.Domain = config.SysConfig.GetHFURLBase()
					afterTasks := getContiguousRanges(response.MaxOffset, endPos, taskParam)
					tasks = append(tasks, beforeTasks...)
					tasks = append(tasks, afterTasks...)
				}
				return tasks
			} else {
				goto localTask
			}
		}
	} else {
		goto localTask
	}

localTask:
	taskParam.Domain = config.SysConfig.GetHFURLBase()
	tasks = getContiguousRanges(startPos, endPos, taskParam)
	return tasks
}

func (d *DownloaderDao) SyncFileProcess(dataType, orgRepo, fileName, etag string, startPos, endPos, fileSize int64) error {
	org, repo := util.SplitOrgRepo(orgRepo)
	err := d.schedulerDao.SyncFileProcess(&manager.SchedulerFileRequest{
		DataType:   dataType,
		Org:        org,
		Repo:       repo,
		Name:       fileName,
		Etag:       etag,
		InstanceId: config.SysConfig.Scheduler.Discovery.InstanceId,
		StartPos:   startPos,
		EndPos:     endPos,
		FileSize:   fileSize,
	})
	if err != nil {
		zap.S().Errorf("SyncFileProcess err.%v", err)
		return err
	}
	return nil
}

func (d *DownloaderDao) getRequestDomainScheduler(dataType, orgRepo, fileName, etag string, startPos, endPos, fileSize int64) (*manager.SchedulerFileResponse, error) {
	org, repo := util.SplitOrgRepo(orgRepo)
	response, err := d.schedulerDao.SchedulerFile(&manager.SchedulerFileRequest{
		DataType:   dataType,
		Org:        org,
		Repo:       repo,
		Name:       fileName,
		Etag:       etag,
		InstanceId: config.SysConfig.Scheduler.Discovery.InstanceId,
		StartPos:   startPos,
		EndPos:     endPos,
		FileSize:   fileSize,
	})
	if err != nil {
		zap.S().Errorf("getSchedulerRequest err.%v", err)
		return nil, err
	}
	return response, nil
}

func getQueueSize(rangeStartPos, rangeEndPos int64) int64 {
	bufSize := min(config.SysConfig.Download.RemoteFileBufferSize, rangeEndPos-rangeStartPos)
	return bufSize/config.SysConfig.Download.RespChunkSize + 1
}

func doTask(ctx context.Context, tasks []common.Task) {
	var pool *common.Pool
	taskLen := len(tasks)
	if taskLen == 0 {
		return
	} else if taskLen >= config.SysConfig.Download.GoroutineMaxNumPerFile {
		pool = common.NewPool(config.SysConfig.Download.GoroutineMaxNumPerFile)
	} else {
		pool = common.NewPool(taskLen)
	}
	defer pool.Close()
	for i := 0; i < taskLen; i++ {
		if ctx.Err() != nil {
			return
		}
		task := tasks[i]
		task.SetTaskSize(taskLen)
		if err := pool.Submit(ctx, task); err != nil {
			zap.S().Errorf("submit task err.%v", err)
			return
		}
		if config.SysConfig.GetRemoteFileRangeWaitTime() != 0 {
			time.Sleep(config.SysConfig.GetRemoteFileRangeWaitTime())
		}
	}
}

func analysisFilePosition(dingFile *downloader.DingCache, startPos, endPos int64) (bool, int64) {
	if startPos == 0 && endPos == 0 {
		return true, endPos
	}
	if startPos < 0 || endPos <= startPos || endPos > dingFile.GetFileSize() {
		zap.S().Errorf("Invalid startPos or endPos: startPos=%d, endPos=%d", startPos, endPos)
		return false, startPos
	}
	startBlock := startPos / dingFile.GetBlockSize()
	endBlock := (endPos - 1) / dingFile.GetBlockSize()
	for curBlock := startBlock; curBlock <= endBlock; curBlock++ {
		blockExists, err := dingFile.HasBlock(curBlock)
		if err != nil {
			zap.S().Errorf("Failed to check block existence: %v", err)
			curPos := curBlock * dingFile.GetBlockSize()
			return false, curPos
		}
		if !blockExists {
			curPos := curBlock * dingFile.GetBlockSize()
			return false, curPos
		}
	}
	return true, endPos
}

// 将文件的偏移量分为cache和remote，对针对remote按照指定的RangeSize做切分

func getContiguousRanges(startPos, endPos int64, taskParam *downloader.TaskParam) (tasks []common.Task) {
	ctx := taskParam.Context
	dingFile := taskParam.DingFile
	if startPos == 0 && endPos == 0 {
		return
	}
	if startPos < 0 || endPos <= startPos || endPos > dingFile.GetFileSize() {
		zap.S().Errorf("Invalid startPos or endPos: startPos=%d, endPos=%d", startPos, endPos)
		return
	}
	startBlock := startPos / dingFile.GetBlockSize()
	endBlock := (endPos - 1) / dingFile.GetBlockSize()

	rangeStartPos, curPos := startPos, startPos
	blockExists, err := dingFile.HasBlock(startBlock)
	if err != nil {
		zap.S().Errorf("Failed to check block existence: %v", err)
		return
	}
	rangeIsRemote := !blockExists // 不存在，从远程获取，为true
	taskNo := taskParam.TaskNo
	for curBlock := startBlock; curBlock <= endBlock; curBlock++ {
		if ctx.Err() != nil {
			return
		}
		_, _, blockEndPos := downloader.GetBlockInfo(curPos, dingFile.GetBlockSize(), dingFile.GetFileSize())
		blockExists, err = dingFile.HasBlock(curBlock)
		if err != nil {
			zap.S().Errorf("HasBlock err. curBlock:%d,curPos:%d, %v", curBlock, curPos, err)
			return
		}
		curIsRemote := !blockExists // 不存在，从远程获取，为true，存在为false。
		if rangeIsRemote != curIsRemote {
			if rangeStartPos < curPos {
				if rangeIsRemote {
					rTasks := splitRemoteRange(rangeStartPos, curPos, &taskNo, taskParam)
					tasks = append(tasks, rTasks...)
				} else {
					c := createCacheTask(taskNo, rangeStartPos, curPos, taskParam)
					tasks = append(tasks, c)
					taskNo++
				}
			}
			rangeStartPos = curPos
			rangeIsRemote = curIsRemote
		}
		curPos = blockEndPos
	}
	if rangeIsRemote {
		rTasks := splitRemoteRange(rangeStartPos, endPos, &taskNo, taskParam)
		tasks = append(tasks, rTasks...)
	} else {
		c := createCacheTask(taskNo, rangeStartPos, endPos, taskParam)
		tasks = append(tasks, c)
		taskNo++
	}
	taskParam.TaskNo = taskNo
	return
}

func splitRemoteRange(startPos, endPos int64, taskNo *int, taskParam *downloader.TaskParam) []common.Task {
	rangeSize := config.SysConfig.Download.RemoteFileRangeSize
	remoteTasks := make([]common.Task, 0)
	if rangeSize == 0 {
		c := createRemoteTask(*taskNo, startPos, endPos, taskParam)
		remoteTasks = append(remoteTasks, c)
		*taskNo++
		return remoteTasks
	}
	for start := startPos; start < endPos; {
		end := start + rangeSize
		if end > endPos {
			end = endPos
		}
		c := createRemoteTask(*taskNo, start, end, taskParam)
		remoteTasks = append(remoteTasks, c)
		*taskNo++
		start = end
	}
	return remoteTasks
}

func createCacheTask(taskNo int, start, end int64, taskParam *downloader.TaskParam) *downloader.CacheFileTask {
	cache := downloader.NewCacheFileTask(taskNo, start, end)
	cache.Context = taskParam.Context
	cache.DingFile = taskParam.DingFile
	cache.TaskSize = taskParam.TaskSize
	cache.FileName = taskParam.FileName
	cache.OrgRepo = taskParam.OrgRepo
	cache.ResponseChan = taskParam.ResponseChan
	cache.Preheat = taskParam.Preheat
	return cache
}

func createRemoteTask(taskNo int, start, end int64, taskParam *downloader.TaskParam) *downloader.RemoteFileTask {
	remote := downloader.NewRemoteFileTask(taskNo, start, end)
	remote.Context = taskParam.Context
	remote.DingFile = taskParam.DingFile
	remote.Authorization = taskParam.Authorization
	remote.Domain = taskParam.Domain
	remote.Uri = taskParam.Uri
	remote.Queue = make(chan []byte, getQueueSize(remote.RangeStartPos, remote.RangeEndPos))
	remote.ResponseChan = taskParam.ResponseChan
	remote.TaskSize = taskParam.TaskSize
	remote.FileName = taskParam.FileName
	remote.OrgRepo = taskParam.OrgRepo
	remote.DataType = taskParam.DataType
	remote.Etag = taskParam.Etag
	remote.Cancel = taskParam.Cancel
	remote.Preheat = taskParam.Preheat
	return remote
}
