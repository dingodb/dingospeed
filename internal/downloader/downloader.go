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

package downloader

import (
	"context"
	"os"
	"time"

	"dingo-hfmirror/pkg/common"
	"dingo-hfmirror/pkg/config"

	"go.uber.org/zap"
)

// 整个文件
func FileDownload(ctx context.Context, hfUrl, savePath, fileName, authorization string, fileSize, startPos, endPos int64, responseChan chan []byte) {
	var dingFile *DingCache
	if _, err := os.Stat(savePath); err == nil {
		if dingFile, err = NewDingCache(savePath, config.SysConfig.Download.BlockSize); err != nil {
			zap.S().Errorf("NewDingCache err.%v", err)
			return
		}
	} else {
		if dingFile, err = NewDingCache(savePath, config.SysConfig.Download.BlockSize); err != nil {
			zap.S().Errorf("NewDingCache err.%v", err)
			return
		}
		if err = dingFile.Resize(fileSize); err != nil {
			zap.S().Errorf("Resize err.%v", err)
			return
		}
	}
	defer dingFile.Close()
	defer close(responseChan)
	tasks := getContiguousRanges(dingFile, startPos, endPos)
	var remoteTasks []*RemoteFileTask
	taskSize := len(tasks)
	for i := 0; i < taskSize; i++ {
		task := tasks[i]
		if remote, ok := task.(*RemoteFileTask); ok {
			remote.Context = ctx
			remote.authorization = authorization
			remote.hfUrl = hfUrl
			remote.DingFile = dingFile
			remote.Queue = make(chan []byte, getQueueSize(remote.RangeStartPos, remote.RangeEndPos))
			remote.ResponseChan = responseChan
			remote.TaskSize = taskSize
			remote.FileName = fileName
			remoteTasks = append(remoteTasks, remote)
		} else if cache, ok := task.(*CacheFileTask); ok {
			cache.DingFile = dingFile
			cache.TaskSize = taskSize
			cache.FileName = fileName
			cache.ResponseChan = responseChan
		}
	}
	if len(remoteTasks) > 0 {
		go startRemoteDownload(ctx, remoteTasks)
	}
	for i := 0; i < len(tasks); i++ {
		task := tasks[i]
		task.OutResult()
	}
}

func getQueueSize(rangeStartPos, rangeEndPos int64) int64 {
	bufSize := min(config.SysConfig.Download.RemoteFileBufferSize, rangeEndPos-rangeStartPos)
	return bufSize/config.SysConfig.Download.RespChunkSize + 1
}

func startRemoteDownload(ctx context.Context, remoteFileTasks []*RemoteFileTask) {
	var pool *common.Pool
	taskLen := len(remoteFileTasks)
	if taskLen == 0 {
		return
	} else if taskLen >= config.SysConfig.Download.GoroutineMaxNumPerFile {
		pool = common.NewPool(config.SysConfig.Download.GoroutineMaxNumPerFile)
	} else {
		pool = common.NewPool(taskLen)
	}
	defer pool.Close()
	remoteFileTasks[0].ResponseChan <- []byte{} // 先建立长连接
	for i := 0; i < taskLen; i++ {
		task := remoteFileTasks[i]
		if err := pool.Submit(ctx, task); err != nil {
			zap.S().Errorf("submit task err.%v", err)
			return
		}
		time.Sleep(config.SysConfig.GetRemoteFileRangeWaitTime())
	}
}

// 将文件的偏移量分为cache和remote，对针对remote按照指定的RangeSize做切分

func getContiguousRanges(dingFile *DingCache, startPos, endPos int64) (tasks []common.Task) {
	if startPos < 0 || endPos <= startPos || endPos > dingFile.GetFileSize() {
		zap.S().Errorf("Invalid startPos or endPos: startPos=%d, endPos=%d", startPos, endPos)
		return
	}
	startBlock := startPos / dingFile.getBlockSize()
	endBlock := (endPos - 1) / dingFile.getBlockSize()

	rangeStartPos, curPos := startPos, startPos
	blockExists, err := dingFile.HasBlock(startBlock)
	if err != nil {
		zap.S().Errorf("Failed to check block existence: %v", err)
		return
	}
	rangeIsRemote := !blockExists // 不存在，从远程获取，为true
	taskNo := 0
	for curBlock := startBlock; curBlock <= endBlock; curBlock++ {
		_, _, blockEndPos := getBlockInfo(curPos, dingFile.getBlockSize(), dingFile.GetFileSize())
		blockExists, err = dingFile.HasBlock(curBlock)
		if err != nil {
			zap.S().Errorf("HasBlock err. curBlock:%d,curPos:%d, %v", curBlock, curPos, err)
			return
		}
		curIsRemote := !blockExists // 不存在，从远程获取，为true，存在为false。
		if rangeIsRemote != curIsRemote {
			if rangeStartPos < curPos {
				if rangeIsRemote {
					tasks = splitRemoteRange(tasks, rangeStartPos, curPos, &taskNo)
				} else {
					c := NewCacheFileTask(taskNo, rangeStartPos, curPos)
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
		tasks = splitRemoteRange(tasks, rangeStartPos, endPos, &taskNo)
	} else {
		c := NewCacheFileTask(taskNo, rangeStartPos, endPos)
		tasks = append(tasks, c)
		taskNo++
	}
	return
}

func splitRemoteRange(tasks []common.Task, startPos, endPos int64, taskNo *int) []common.Task {
	rangeSize := config.SysConfig.Download.RemoteFileRangeSize
	if rangeSize == 0 {
		c := NewRemoteFileTask(*taskNo, startPos, endPos)
		tasks = append(tasks, c)
		return tasks
	}
	for start := startPos; start < endPos; {
		end := start + rangeSize
		if end > endPos {
			end = endPos
		}
		c := NewRemoteFileTask(*taskNo, start, end)
		tasks = append(tasks, c)
		*taskNo++
		start = end
	}
	return tasks
}

// get_block_info 函数
func getBlockInfo(pos, blockSize, fileSize int64) (int64, int64, int64) {
	curBlock := pos / blockSize
	blockStartPos := curBlock * blockSize
	blockEndPos := min((curBlock+1)*blockSize, fileSize)
	return curBlock, blockStartPos, blockEndPos
}
