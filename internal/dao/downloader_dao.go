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
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"dingospeed/internal/data"
	"dingospeed/internal/downloader"
	"dingospeed/pkg/common"
	"dingospeed/pkg/config"
	"dingospeed/pkg/consts"
	myerr "dingospeed/pkg/error"
	"dingospeed/pkg/proto/manager"
	"dingospeed/pkg/repository"

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
func (d *DownloaderDao) FileDownload(startPos, endPos int64, isInnerRequest bool, taskParam *downloader.TaskParam) error {
	key, identityErr := repository.ParseID(taskParam.DataType, taskParam.OrgRepo)
	if identityErr != nil {
		return identityErr
	}
	taskParam.RepoKey = key
	if !taskParam.LocalOnly && key.Namespace == repository.HuggingFace && config.SysConfig.IsCluster() && taskParam.FileSize > config.SysConfig.GetMinimumFileSize() {
		taskParam.OnCacheComplete = func() { d.reconcileCachedFile(taskParam) }
	}
	dingCacheManager := downloader.GetInstance()
	dingFile, err := dingCacheManager.GetDingFile(taskParam.BlobsFile, taskParam.FileSize)
	if err != nil {
		zap.S().Errorf("GetDingFile err.%v", err)
		return myerr.NewAppendCode(http.StatusInternalServerError, "Get DingFile err")
	}
	taskParam.DingFile = dingFile
	tasks, err := d.constructTask(startPos, endPos, isInnerRequest, taskParam)
	if err != nil {
		dingCacheManager.ReleasedDingFile(taskParam.BlobsFile)
		return err
	}
	// Both OutResult and DoTask read TaskSize. Initialize it before either starts.
	for _, task := range tasks {
		task.SetTaskSize(len(tasks))
	}
	go func() {

		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer func() {
				wg.Done()
			}()
			drainOutputs(taskParam.Context, tasks, taskParam.Unordered)
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
		complete, _ := analysisFilePosition(dingFile, 0, taskParam.FileSize)
		dingCacheManager.ReleasedDingFile(taskParam.BlobsFile)
		close(taskParam.ResponseChan)
		if taskParam.CacheResult != nil {
			var result error
			if !complete {
				result = fmt.Errorf("file cache incomplete")
				if taskParam.Context.Err() != nil {
					result = taskParam.Context.Err()
				}
			}
			for _, child := range tasks {
				if remote, ok := child.(*downloader.RemoteFileTask); ok && remote.ResultError != nil && !errors.Is(remote.ResultError, context.Canceled) {
					result = remote.ResultError
					break
				}
			}
			taskParam.CacheResult <- result
		}
	}()
	return nil
}

func (d *DownloaderDao) constructTask(startPos, endPos int64, isInnerRequest bool, taskParam *downloader.TaskParam) ([]common.DownloadTask, error) {
	var (
		tasks        []common.DownloadTask
		ctx          = taskParam.Context
		fileComplete bool
		curPos       int64
	)
	// 小于这个值的文件将不参与调度
	if taskParam.FileSize <= config.SysConfig.GetMinimumFileSize() {
		goto localTask
	}
	// 分析下载类型是否全部存在，若文件不完整，返回当前已缓存的最大偏移量
	fileComplete, curPos = analysisFilePosition(taskParam.DingFile, startPos, endPos)
	if !fileComplete && !config.SysConfig.Online() { // 文件不完整，且当前节点为离线
		return nil, myerr.NewAppendCode(http.StatusNotFound, "Entry not found")
	}
	// isInnerRequest为true，即内部请求，是已经被调度过后，设置为内部域名的请求，这种请求将不会再次参与调度，直接做下载即可。
	if !taskParam.LocalOnly && !isInnerRequest && config.SysConfig.IsCluster() && !fileComplete {
		if response, err := d.getRequestDomainScheduler(taskParam.DataType, taskParam.OrgRepo, taskParam.FileName, taskParam.Etag, curPos, endPos, taskParam.FileSize); err != nil {
			zap.S().Errorf("getRequestDomainScheduler err.%v", err)
			goto localTask
		} else {
			ctx = context.WithValue(ctx, consts.KeyProcessId, response.ProcessId)
			ctx = context.WithValue(ctx, consts.KeyMasterInstanceId, response.MasterInstanceId)
			taskParam.Context = ctx
			if response.SchedulerType == consts.SchedulerYes {
				if curPos > 0 && startPos < curPos {
					tasks = getContiguousRanges(startPos, curPos, taskParam)
				}
				speedDomain := fmt.Sprintf("http://%s:%d", response.Host, response.Port) // 此刻向该节点发起远程下载请求
				if endPos <= response.MaxOffset {
					taskParam.Peer = true
					taskParam.Domain = speedDomain
					speedTasks := getContiguousRanges(curPos, endPos, taskParam)
					tasks = append(tasks, speedTasks...)
				} else {
					// 需要重新拆分任务
					taskParam.Peer = true
					taskParam.Domain = speedDomain
					beforeTasks := getContiguousRanges(curPos, response.MaxOffset, taskParam)
					tasks = append(tasks, beforeTasks...)
					taskParam.Peer = false
					taskParam.Domain = upstreamDomain(taskParam)
					afterTasks := getContiguousRanges(response.MaxOffset, endPos, taskParam)
					tasks = append(tasks, afterTasks...)
				}
				return tasks, nil
			} else {
				goto localTask
			}
		}
	} else {
		goto localTask
	}

localTask:
	taskParam.Peer = false
	taskParam.Domain = upstreamDomain(taskParam)
	tasks = getContiguousRanges(startPos, endPos, taskParam)
	return tasks, nil
}

func upstreamDomain(p *downloader.TaskParam) string {
	if p.Source != nil {
		return p.Source.Domain
	}
	return config.SysConfig.GetHFURLBase()
}

func (d *DownloaderDao) getRequestDomainScheduler(dataType, orgRepo, fileName, etag string, startPos, endPos, fileSize int64) (*manager.SchedulerFileResponse, error) {
	key, err := repository.ParseID(dataType, orgRepo)
	if err != nil {
		return nil, err
	}
	org, repo := key.Namespace, key.Repo
	response, err := d.schedulerDao.SchedulerFile(&manager.SchedulerFileRequest{
		DataType:   dataType,
		Org:        org,
		Repo:       repo,
		Name:       fileName,
		Etag:       etag,
		InstanceId: config.SysConfig.Registration().NodeID,
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

func doTask(ctx context.Context, tasks []common.DownloadTask) {
	var pool *common.Pool
	taskLen := len(tasks)
	if taskLen == 0 {
		return
	} else if taskLen >= config.SysConfig.Download.GoroutineMaxNumPerFile {
		pool = common.NewPool(config.SysConfig.Download.GoroutineMaxNumPerFile, false)
	} else {
		pool = common.NewPool(taskLen, false)
	}
	defer pool.Close()
	for i := 0; i < taskLen; i++ {
		if ctx.Err() != nil {
			return
		}
		task := tasks[i]
		if err := pool.Submit(ctx, task); err != nil {
			zap.S().Errorf("submit task err.%v", err)
			return
		}
		if config.SysConfig.GetRemoteFileRangeWaitTime() != 0 {
			timer := time.NewTimer(config.SysConfig.GetRemoteFileRangeWaitTime())
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return
			}
		}
	}
}

func analysisFilePosition(dingFile *downloader.DingCache, startPos, endPos int64) (bool, int64) {
	if startPos == 0 && endPos == 0 {
		return true, startPos
	}
	if startPos < 0 || endPos <= startPos || endPos > dingFile.GetFileSize() {
		zap.S().Errorf("Invalid startPos/endPos: path=%s, startPos=%d, endPos=%d, filesize:%d", dingFile.GetPath(), startPos, endPos, dingFile.GetFileSize())
		return false, startPos
	}
	startBlock := startPos / dingFile.GetBlockSize()
	endBlock := (endPos - 1) / dingFile.GetBlockSize()
	for curBlock := startBlock; curBlock <= endBlock; curBlock++ {
		blockExists, err := dingFile.HasBlock(curBlock)
		if err != nil {
			zap.S().Errorf("Failed to check block existence: %v", err)
		}
		if !blockExists {
			curPos := curBlock * dingFile.GetBlockSize()
			if curPos < startPos { // 若startPos就不存在，将直接返回该位置。
				curPos = startPos
			}
			return false, curPos
		}
	}
	return true, endPos
}

// 将文件的偏移量分为cache和remote，对针对remote按照指定的RangeSize做切分

func getContiguousRanges(startPos, endPos int64, taskParam *downloader.TaskParam) (tasks []common.DownloadTask) {
	ctx := taskParam.Context
	dingFile := taskParam.DingFile
	if startPos == 0 && endPos == 0 {
		return
	}
	if startPos < 0 || endPos <= startPos || (endPos-1) > dingFile.GetFileSize() {
		zap.S().Errorf("Invalid pos path=%s, startPos=%d, endPos=%d", dingFile.GetPath(), startPos, endPos)
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

func splitRemoteRange(startPos, endPos int64, taskNo *int, taskParam *downloader.TaskParam) []common.DownloadTask {
	rangeSize := config.SysConfig.Download.RemoteFileRangeSize
	remoteTasks := make([]common.DownloadTask, 0)
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
	cache.RepoKey = taskParam.RepoKey
	cache.OrgRepo = taskParam.RepoKey.ID()
	cache.ResponseChan = taskParam.ResponseChan
	cache.OnComplete = taskParam.OnCacheComplete
	return cache
}

// Sync by file identity, without scheduling another download or resetting an
// existing process. StartPos equals the verified full prefix, so the existing
// monotonic SQL guard accepts both missing and partially reported progress.
func (d *DownloaderDao) reconcileCachedFile(p *downloader.TaskParam) {
	entry := &manager.FileProcessEntry{
		DataType: p.DataType, Org: p.RepoKey.Namespace, Repo: p.RepoKey.Repo,
		Name: p.FileName, Etag: p.Etag, FileSize: p.FileSize,
		InstanceId: config.SysConfig.Registration().NodeID,
		StartPos:   p.FileSize, EndPos: p.FileSize, Status: consts.StatusDownloaded,
	}
	if err := d.schedulerDao.SyncFileProcess(&manager.SyncFileProcessReq{FileProcessEntries: []*manager.FileProcessEntry{entry}}); err != nil {
		zap.S().Errorf("reconcile cached file %s/%s: %v", p.OrgRepo, p.FileName, err)
		data.WriteLocalOperationChan(consts.OperationProcess, &data.FileProcessParam{
			Datatype: p.DataType, Org: p.RepoKey.Namespace, Repo: p.RepoKey.Repo,
			Name: p.FileName, Etag: p.Etag, FileSize: p.FileSize,
			StartPos: p.FileSize, EndPos: p.FileSize, Status: consts.StatusDownloaded,
		})
	}
}

func createRemoteTask(taskNo int, start, end int64, taskParam *downloader.TaskParam) *downloader.RemoteFileTask {
	remote := downloader.NewRemoteFileTask(taskNo, start, end)
	remote.Source = taskParam.Source
	remote.LocalOnly = taskParam.LocalOnly
	remote.Context = taskParam.Context
	remote.DingFile = taskParam.DingFile
	remote.Authorization = taskParam.Authorization
	remote.Domain = taskParam.Domain
	remote.Uri = taskParam.Uri
	remote.UpstreamURI = taskParam.Uri
	remote.Peer = taskParam.Peer
	if taskParam.Peer {
		remote.Uri = peerFileURI(taskParam)
	}
	remote.Queue = make(chan []byte, getQueueSize(remote.RangeStartPos, remote.RangeEndPos))
	remote.ResponseChan = taskParam.ResponseChan
	remote.TaskSize = taskParam.TaskSize
	remote.FileName = taskParam.FileName
	remote.RepoKey = taskParam.RepoKey
	remote.OrgRepo = taskParam.RepoKey.ID()
	remote.DataType = taskParam.DataType
	remote.Etag = taskParam.Etag
	remote.Cancel = taskParam.Cancel
	return remote
}

func peerFileURI(p *downloader.TaskParam) string {
	if p.RepoKey.Namespace == repository.HuggingFace {
		prefix := ""
		if p.RepoKey.RepoType != "models" {
			prefix = "/" + p.RepoKey.RepoType
		}
		return prefix + "/" + repository.EscapeURLPath(p.RepoKey.Repo) + "/resolve/" + url.PathEscape(p.Revision) + "/" + repository.EscapeURLPath(p.FileName)
	}

	q := url.Values{"repo": {p.RepoKey.Repo}, "revision": {p.Revision}, "path": {p.FileName}}
	return "/api/repositories/" + url.PathEscape(p.RepoKey.RepoType) + "/" + url.PathEscape(p.RepoKey.Namespace) + "/file?" + q.Encode()
}

// drainOutputs moves every range task's output into the shared response channel.
//
// A client needs the file in byte order, so ranges are drained one after
// another. The cost is that a later range can buffer only
// download.remoteFileBufferSize before it blocks behind the ranges ahead of it,
// which serialises a split download back into roughly one stream per file.
// Consumers that ignore order (Unordered) drain every range concurrently so
// each range keeps its connection busy.
func drainOutputs(ctx context.Context, tasks []common.DownloadTask, unordered bool) {
	if len(tasks) == 0 || ctx.Err() != nil {
		return
	}
	select {
	case tasks[0].GetResponseChan() <- []byte{}:
	case <-ctx.Done():
		return
	}
	if !unordered {
		for _, task := range tasks {
			if ctx.Err() != nil {
				return
			}
			task.OutResult()
		}
		return
	}
	var wg sync.WaitGroup
	for _, task := range tasks {
		wg.Add(1)
		go func(task common.DownloadTask) {
			defer wg.Done()
			task.OutResult()
		}(task)
	}
	wg.Wait()
}
