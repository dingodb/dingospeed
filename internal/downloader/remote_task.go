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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"

	"dingospeed/internal/data"
	"dingospeed/pkg/common"
	"dingospeed/pkg/config"
	"dingospeed/pkg/consts"
	myerr "dingospeed/pkg/error"
	"dingospeed/pkg/prom"
	"dingospeed/pkg/repository"
	"dingospeed/pkg/util"

	"go.uber.org/zap"
)

type RemoteFileTask struct {
	Source      *RemoteSource
	LocalOnly   bool
	Peer        bool
	UpstreamURI string
	*DownloadTask
	Authorization string
	Domain        string
	Uri           string
	DataType      string
	Etag          string
	Queue         chan []byte `json:"-"`
	Cancel        context.CancelFunc
	ResultError   error
}

func NewRemoteFileTask(taskNo int, rangeStartPos int64, rangeEndPos int64) *RemoteFileTask {
	r := &RemoteFileTask{}
	r.DownloadTask = &DownloadTask{}
	r.TaskNo = taskNo
	r.RangeStartPos = rangeStartPos
	r.RangeEndPos = rangeEndPos
	return r
}

// 分段下载
func (r *RemoteFileTask) DoTask() {
	var (
		curBlock    int64
		wg          sync.WaitGroup
		streamCache = bytes.Buffer{}
		fetchErr    error
		cacheErr    error
	)
	contentChan := make(chan []byte, consts.RespChanSize)
	rangeStartPos, rangeEndPos := r.RangeStartPos, r.RangeEndPos
	zap.S().Infof("start remote dotask:%s/%s, taskNo:%d, size:%d, domain:%s, startPos:%d, endPos:%d", r.OrgRepo, r.FileName, r.TaskNo, r.TaskSize, r.Domain, rangeStartPos, rangeEndPos)
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer close(contentChan)
		fetchErr = r.getFileRangeFromRemote(rangeStartPos, rangeEndPos, contentChan)
		if err := fetchErr; err != nil {
			zap.S().Errorf("getFileRangeFromRemote err.%v", err)
			r.Cancel()
		}
	}()
	curPos, lastReportPos := rangeStartPos, rangeStartPos
	lastBlock, lastBlockStartPos, lastBlockEndPos := GetBlockInfo(curPos, r.DingFile.GetBlockSize(), r.DingFile.GetFileSize()) // 块编号，开始位置，结束位置
	blockNumber := r.DingFile.getBlockNumber()
	go func() {
		defer func() {
			close(r.Queue)
			wg.Done()
		}()
		var interval int64 = 1
		for {
			select {
			case chunk, ok := <-contentChan:
				{
					if !ok {
						return
					}
					select {
					case r.Queue <- chunk:
					case <-r.Context.Done():
						zap.S().Warnf("send chunk err:%s/%s, task %d, ctx done, DoTask exit.", r.OrgRepo, r.FileName, r.TaskNo)
						return
					}
					chunkLen := int64(len(chunk))
					curPos += chunkLen
					if config.SysConfig.EnableMetric() {
						// 原子性地更新总下载字节数
						source := util.Itoa(r.Context.Value(consts.PromSource))
						prom.PromRequestByteCounter(prom.RequestRemoteByte, source, r.OrgRepo, r.Domain, chunkLen)
					}

					if cacheErr != nil {
						continue // Cache failure must not cancel delivery of upstream bytes.
					}
					if len(chunk) != 0 {
						streamCache.Write(chunk)
					}
					curBlock = curPos / r.DingFile.GetBlockSize()
					// 若是一个新的数据块，则将上一个数据块持久化。
					for curBlock > lastBlock && (lastBlock+1)*r.DingFile.GetBlockSize() <= r.DingFile.GetFileSize() && cacheErr == nil {
						splitPos := lastBlockEndPos - max(lastBlockStartPos, rangeStartPos)
						cacheLen := int64(streamCache.Len())
						if splitPos > cacheLen {
							// 正常不会出现splitPos>len(streamCacheBytes),若出现只能降级处理。
							zap.S().Errorf("splitPos err.%d-%d", splitPos, cacheLen)
							splitPos = cacheLen
						}
						streamCacheBytes := streamCache.Bytes()
						rawBlock := streamCacheBytes[:splitPos] // 当前块的数据
						if int64(len(rawBlock)) == r.DingFile.GetBlockSize() {
							hasBlockBool, err := r.DingFile.HasBlock(lastBlock)
							if err != nil {
								zap.S().Errorf("HasBlock err.%v", err)
								cacheErr = err
							}
							if err == nil && !hasBlockBool {
								if err = r.writeCacheBlock(lastBlock, rawBlock); err != nil {
									zap.S().Errorf("writeBlock err.%v", err)
									cacheErr = err
									continue
								}
								zap.S().Debugf("from:%s, %s/%s, taskNo:%d, block：%d(%d)write done, range：%d-%d.", r.Domain, r.OrgRepo, r.FileName, r.TaskNo, lastBlock, blockNumber, lastBlockStartPos, lastBlockEndPos)
								if !r.LocalOnly && interval == config.SysConfig.GetSyncProcessInterval() {
									data.ReportFileProcess(r.Context, r.constructFileProcessParam(lastReportPos, lastBlockEndPos, consts.StatusDownloading))
									lastReportPos = lastBlockEndPos
									interval = 1
								} else {
									interval++
								}
							}
						}
						nextBlock := streamCacheBytes[splitPos:] // 下一个块的数据
						streamCache.Truncate(0)
						streamCache.Write(nextBlock)
						lastBlock, lastBlockStartPos, lastBlockEndPos = GetBlockInfo(lastBlockEndPos, r.DingFile.GetBlockSize(), r.DingFile.GetFileSize())
					}
				}
			case <-r.Context.Done():
				zap.S().Warnf("file:%s/%s taskNo:%d ctx done, DoTask exit.", r.OrgRepo, r.FileName, r.TaskNo)
				return
			}
		}
	}()
	wg.Wait()
	defer func() {
		if cacheErr != nil {
			r.ResultError = cacheErr
		} else if fetchErr != nil {
			r.ResultError = fetchErr
		}
	}()
	// Report exactly one terminal outcome, after both transfer and cache writes finish.
	completed := false
	defer func() {
		status := int32(consts.StatusDownloadBreak)
		if completed {
			status = consts.StatusDownloaded
		}
		if !r.LocalOnly {
			data.ReportFileProcess(r.Context, r.constructFileProcessParam(lastReportPos, rangeEndPos, status))
		}
	}()
	if fetchErr != nil || cacheErr != nil || curPos != rangeEndPos {
		return
	}
	rawBlock := streamCache.Bytes()
	if curBlock == r.DingFile.getBlockNumber()-1 {
		// 对不足一个block的数据做补全
		if int64(len(rawBlock)) == r.DingFile.GetFileSize()%r.DingFile.GetBlockSize() {
			padding := bytes.Repeat([]byte{0}, int(r.DingFile.GetBlockSize())-len(rawBlock))
			rawBlock = append(rawBlock, padding...)
		}
		lastBlock = curBlock
	}
	// Commit the remaining block before checking cache coverage.
	if int64(len(rawBlock)) == r.DingFile.GetBlockSize() {
		hasBlockBool, err := r.DingFile.HasBlock(lastBlock)
		if err != nil {
			cacheErr = err
			zap.S().Errorf("HasBlock err.%v", err)
			return
		}
		if !hasBlockBool {
			if err = r.writeCacheBlock(lastBlock, rawBlock); err != nil {
				cacheErr = err
				zap.S().Errorf("last writeBlock err.%v", err)
				return
			}
			zap.S().Debugf("from:%s, %s/%s, taskNo:%d, last block：%d(%d)write done, range：%d-%d.", r.Domain, r.OrgRepo, r.FileName, r.TaskNo, lastBlock, blockNumber, lastBlockStartPos, lastBlockEndPos)
		}
	}
	// Receiving all bytes alone does not establish that the requested cache range exists.
	for block := rangeStartPos / r.DingFile.GetBlockSize(); block*r.DingFile.GetBlockSize() < rangeEndPos; block++ {
		has, err := r.DingFile.HasBlock(block)
		if err != nil || !has {
			zap.S().Errorf("remote cache incomplete: block=%d, err=%v", block, err)
			return
		}
	}
	// A client may close its connection after receiving Content-Length bytes while
	// the final cache write is still running. Completed cache coverage survives
	// that cancellation; incomplete transfers and failed writes are rejected above.
	completed = true
	zap.S().Infof("end remote dotask:%s/%s, taskNo:%d, size:%d, domain:%s, startPos:%d, endPos:%d", r.OrgRepo, r.FileName, r.TaskNo, r.TaskSize, r.Domain, rangeStartPos, rangeEndPos)
}

func (r *RemoteFileTask) constructFileProcessParam(startPos, endPos int64, status int32) *data.FileProcessParam {
	org, repo := r.RepoKey.Namespace, r.RepoKey.Repo
	return &data.FileProcessParam{
		Datatype: r.DataType,
		Org:      org,
		Repo:     repo,
		Name:     r.FileName,
		Etag:     r.Etag,
		FileSize: r.DingFile.GetFileSize(),
		StartPos: startPos,
		EndPos:   endPos,
		Status:   status,
	}
}

func (r *RemoteFileTask) OutResult() {
	for {
		select {
		case chunk, ok := <-r.Queue:
			if !ok {
				zap.S().Debugf("close remote outResult. taskNo:%d, %s/%s", r.TaskNo, r.OrgRepo, r.FileName)
				return
			}
			select {
			case r.ResponseChan <- chunk:
			case <-r.Context.Done():
				zap.S().Debugf("end remote outResult Context.Done() %s/%s", r.OrgRepo, r.FileName)
				return
			}
		case <-r.Context.Done():
			zap.S().Debugf("close remote outResult fileName:%s/%s,err:%v", r.OrgRepo, r.FileName, r.Context.Err())
			return
		}
	}
}

func (r *RemoteFileTask) GetResponseChan() chan []byte {
	return r.ResponseChan
}

func (r *RemoteFileTask) getFileRangeFromRemote(startPos, endPos int64, contentChan chan<- []byte) error {
	var (
		rawData         []byte
		chunkByteLen    = 0
		attempts        = 2
		contentEncoding = ""
		err             error
		n               int
		headers         = make(map[string]string)
	)
	if r.Source != nil {
		for k, v := range r.Source.Headers {
			headers[k] = v
		}
	}
	if r.Authorization != "" {
		headers["authorization"] = r.Authorization
	}
	if startPos > 0 || endPos < r.DingFile.GetFileSize() {
		headers["range"] = fmt.Sprintf("bytes=%d-%d", startPos, endPos-1)
	}
	for i := 0; i < attempts; {
		if err = util.RetryDownloadContext(r.Context, func() error {
			trace := common.CacheTrace(r.Context)
			if !trace.Begin() {
				return context.Canceled
			}
			defer func() { trace.End(r.Context.Err() != nil) }()

			fetch := func(domain, uri string, headers map[string]string, cb func(*http.Response) error) error {
				return util.GetStreamContext(r.Context, domain, uri, headers, cb)
			}
			if r.Peer {
				fetch = func(domain, uri string, headers map[string]string, cb func(*http.Response) error) error {
					return util.GetPeerStreamContext(r.Context, domain, uri, headers, cb)
				}
			} else if r.Source != nil && r.Source.Fetch != nil {
				fetch = func(domain, uri string, headers map[string]string, cb func(*http.Response) error) error {
					return r.Source.Fetch(r.Context, domain, uri, headers, cb)
				}
			}
			err = fetch(r.Domain, r.Uri, headers, func(resp *http.Response) error {
				if headers["range"] != "" && resp.StatusCode == http.StatusOK {
					return fmt.Errorf("source ignored Range request")
				}
				contentEncoding = resp.Header.Get("content-encoding")
				code := resp.StatusCode
				if code != http.StatusOK && code != http.StatusPartialContent {
					if code == http.StatusNotFound {
						zap.S().Errorf("The resource was not found. %s", r.OrgRepo)
					} else if code == http.StatusUnauthorized || code == http.StatusForbidden {
						zap.S().Errorf("Do not have access to this resource. %s", r.OrgRepo)
					} else {
						zap.S().Errorf("Failed resource request.(%d) %s", code, r.OrgRepo)
					}
					return myerr.NewAppendCode(code, fmt.Sprintf("upstream returned HTTP %d", code))
				}
				for {
					select {
					case <-r.Context.Done():
						return nil
					default:
						chunk := make([]byte, config.SysConfig.Download.RespChunkSize)
						n, err = resp.Body.Read(chunk)
						if n > 0 {
							if contentEncoding != "" { // 数据有编码，先收集，后面解码
								rawData = append(rawData, chunk[:n]...)
							} else {
								select {
								case contentChan <- chunk[:n]:
								case <-r.Context.Done():
									// 包装 ctx.Err() 而非返回裸字符串，
									// 上层（如代理池计分）要靠 errors.Is 把
									// 「客户端主动取消」和「出口故障」区分开。
									return fmt.Errorf("form remote ctx done: %w", r.Context.Err())
								}
							}
							chunkByteLen += n // 原始数量
						}
						if err != nil {
							if err == io.EOF {
								if int64(chunkByteLen) < (endPos - startPos) {
									// 数据不完整，将EOF视为读取错误以触发重试/断点续传
									zap.S().Errorf("file:%s/%s, taskNo:%d, premature EOF: expected %d bytes, got %d", r.OrgRepo, r.FileName, r.TaskNo, endPos-startPos, chunkByteLen)
									headers["range"] = fmt.Sprintf("bytes=%d-%d", startPos+int64(chunkByteLen), endPos-1)
									return fmt.Errorf("premature EOF: expected %d bytes, got %d: %w", endPos-startPos, chunkByteLen, io.ErrUnexpectedEOF)
								}
								return nil
							}
							zap.S().Errorf("file:%s/%s, taskNo:%d, statusCode:%d, chunkByteLen:%d, %v", r.OrgRepo, r.FileName, r.TaskNo, resp.StatusCode, chunkByteLen, err)
							if chunkByteLen > 0 {
								headers["range"] = fmt.Sprintf("bytes=%d-%d", startPos+int64(chunkByteLen), endPos-1)
							}
							return err
						}
					}
				}
			})
			return err
		}); err != nil {
			if r.Context.Err() != nil {
				return r.Context.Err()
			}
			var t myerr.Error
			if errors.As(err, &t) {
				break
			}
			// 若从内部其他节点获取数据出现异常，则切换到对应 provider 的官网获取。
			if r.Peer && (r.RepoKey.Namespace == repository.HuggingFace || r.Source != nil) {
				officialDomain := config.SysConfig.GetHFURLBase()
				if r.Source != nil {
					officialDomain = r.Source.Domain
				}
				zap.S().Infof("request fail %s/%s req from %s to %s", r.OrgRepo, r.FileName, r.Domain, officialDomain)
				r.Domain = officialDomain
				r.Uri = r.UpstreamURI
				r.Peer = false
				if chunkByteLen > 0 {
					headers["range"] = fmt.Sprintf("bytes=%d-%d", startPos+int64(chunkByteLen), endPos-1)
				}
				i++
			} else {
				break
			}
		} else {
			break // 访问无异常直接退出
		}
	}
	if err != nil {
		return fmt.Errorf("GetStream: %w", err)
	}
	if contentEncoding != "" {
		// 这里需要实现解压缩逻辑
		finalData, err := util.DecompressData(rawData, contentEncoding)
		if err != nil {
			zap.S().Errorf("DecompressData err.%v", err)
			return err
		}
		select {
		case contentChan <- finalData:
		case <-r.Context.Done():
			return r.Context.Err()
		}
		chunkByteLen = len(finalData) // 将解码后的长度复制为原始的chunkByteLen
	}
	expectedLength := endPos - startPos
	if expectedLength != int64(chunkByteLen) {
		return fmt.Errorf("file:%s/%s, taskNo:%d,The block is incomplete. Expected-%d. Accepted-%d", r.OrgRepo, r.FileName, r.TaskNo, expectedLength, chunkByteLen)
	}
	return nil
}

func (r *RemoteFileTask) writeCacheBlock(block int64, data []byte) error {
	if err := r.DingFile.WriteBlock(block, data); err != nil {
		return err
	}
	common.CacheTrace(r.Context).Wrote(int64(len(data)))
	return nil
}
