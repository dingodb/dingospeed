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
	"fmt"
	"math/rand"
	"os"
	"strings"
	"time"

	"dingo-hfmirror/pkg/common"
	"dingo-hfmirror/pkg/config"
	"dingo-hfmirror/pkg/consts"
	"dingo-hfmirror/pkg/util"

	"go.uber.org/zap"
)

// 整个文件
func FileDownload(ctx context.Context, hfUrl, savePath string, fileSize int64, reqHeaders map[string]string, responseChan chan []byte) {
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
	if len(reqHeaders) > 0 {
		zap.S().Debugf("reqHeaders data:%v", reqHeaders)
	}
	var headRange = reqHeaders["range"]
	if headRange == "" {
		headRange = fmt.Sprintf("bytes=%d-%d", 0, fileSize-1)
	}
	startPos, endPos := parseRangeParams(headRange, fileSize)
	endPos++
	tasks := getContiguousRanges(dingFile, startPos, endPos)
	var remoteTasks []*RemoteFileTask
	for i := 0; i < len(tasks); i++ {
		task := tasks[i]
		if remote, ok := task.(*RemoteFileTask); ok {
			remote.Context = ctx
			remote.authorization = reqHeaders["authorization"]
			remote.hfUrl = hfUrl
			remote.DingFile = dingFile
			remote.Queue = make(chan []byte, consts.RespChanSize)
			remote.ResponseChan = responseChan
			remoteTasks = append(remoteTasks, remote)
		} else if cache, ok := task.(*CacheFileTask); ok {
			cache.DingFile = dingFile
			cache.ResponseChan = responseChan
		}
	}
	if len(remoteTasks) > 0 {
		go startRemoteDownload(ctx, remoteTasks, hfUrl)
	}
	for i := 0; i < len(tasks); i++ {
		task := tasks[i]
		task.OutResult()
	}
}

func startRemoteDownload(ctx context.Context, remoteFileTasks []*RemoteFileTask, hfUrl string) {
	var pool *common.Pool
	zap.S().Debugf("start remote download, %s, task size : %d", hfUrl, len(remoteFileTasks))
	taskLen := len(remoteFileTasks)
	if taskLen >= config.SysConfig.Download.GoroutineMaxNumPerFile {
		pool = common.NewPool(config.SysConfig.Download.GoroutineMaxNumPerFile)
	} else {
		pool = common.NewPool(config.SysConfig.Download.GoroutineMinNumPerFile)
	}
	defer pool.Close()
	rand.Seed(time.Now().UnixNano())
	for i := 0; i < taskLen; i++ {
		task := remoteFileTasks[i]
		if err := pool.Submit(ctx, task); err != nil {
			zap.S().Errorf("submit task err.%v", err)
			return
		}
		if i == 0 {
			time.Sleep(3 * time.Second) // 优先让前三个先下载
		} else {
			time.Sleep(time.Duration(rand.Intn(3)) * time.Second)
		}
	}
}

func parseRangeParams(fileRange string, fileSize int64) (int64, int64) {
	if strings.Contains(fileRange, "/") {
		split := strings.SplitN(fileRange, "/", 2)
		fileRange = split[0]
	}
	if strings.HasPrefix(fileRange, "bytes=") {
		fileRange = fileRange[6:]
	}
	parts := strings.Split(fileRange, "-")
	if len(parts) != 2 {
		panic("file range err.")
	}
	var startPos, endPos int64
	if len(parts[0]) != 0 {
		startPos = util.Atoi64(parts[0])
	} else {
		startPos = 0
	}
	if len(parts[1]) != 0 {
		endPos = util.Atoi64(parts[1])
	} else {
		endPos = fileSize - 1
	}
	return startPos, endPos
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
					c := NewCacheFileTask(taskNo, rangeStartPos, endPos)
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
		tasks = splitRemoteRange(tasks, rangeStartPos, curPos, &taskNo)
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
