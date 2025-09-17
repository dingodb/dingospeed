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
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"

	"dingospeed/internal/data"
	"dingospeed/pkg/common"
	"dingospeed/pkg/config"
	"dingospeed/pkg/consts"
	"dingospeed/pkg/prom"
	"dingospeed/pkg/util"

	"go.uber.org/zap"
)

type RemoteFileTask struct {
	*DownloadTask
	Authorization string
	Domain        string
	Uri           string
	DataType      string
	Etag          string
	Queue         chan []byte `json:"-"`
	Cancel        context.CancelFunc
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
		curBlock int64
		wg       sync.WaitGroup
	)
	contentChan := make(chan []byte, consts.RespChanSize)
	rangeStartPos, rangeEndPos := r.RangeStartPos, r.RangeEndPos
	zap.S().Infof("start remote dotask:%s/%s, taskNo:%d, size:%d, domain:%s, startPos:%d, endPos:%d", r.OrgRepo, r.FileName, r.TaskNo, r.TaskSize, r.Domain, rangeStartPos, rangeEndPos)
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := r.getFileRangeFromRemote(rangeStartPos, rangeEndPos, contentChan); err != nil {
			r.Cancel()
		} else {
			close(contentChan)
		}
	}()
	curPos, lastReportPos := rangeStartPos, rangeStartPos
	streamCache := bytes.Buffer{}
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
					// 先写到缓存
					if !r.Preheat {
						select {
						case r.Queue <- chunk:
						case <-r.Context.Done():
							zap.S().Warnf("send chunk err:%s/%s, task %d, ctx done, DoTask exit.", r.OrgRepo, r.FileName, r.TaskNo)
							data.ReportFileProcess(r.Context, 0, 0, consts.StatusDownloadBreak)
							return
						}
					}
					chunkLen := int64(len(chunk))
					curPos += chunkLen
					if config.SysConfig.EnableMetric() {
						// 原子性地更新总下载字节数
						source := util.Itoa(r.Context.Value(consts.PromSource))
						prom.PromRequestByteCounter(prom.RequestRemoteByte, source, chunkLen, r.OrgRepo)
					}

					if len(chunk) != 0 {
						streamCache.Write(chunk)
					}
					curBlock = curPos / r.DingFile.GetBlockSize()
					// 若是一个新的数据块，则将上一个数据块持久化。
					if curBlock != lastBlock {
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
							}
							if err == nil && !hasBlockBool {
								if err = r.DingFile.WriteBlock(lastBlock, rawBlock); err != nil {
									zap.S().Errorf("writeBlock err.%v", err)
								}
								zap.S().Debugf("from:%s, %s/%s, taskNo:%d, block：%d(%d)write done, range：%d-%d.", r.Domain, r.OrgRepo, r.FileName, r.TaskNo, lastBlock, blockNumber, lastBlockStartPos, lastBlockEndPos)
								if interval == config.SysConfig.GetSyncProcessInterval() {
									data.ReportFileProcess(r.Context, lastReportPos, lastBlockEndPos, consts.StatusDownloading)
									interval = 1
								} else {
									interval++
								}
								lastReportPos = lastBlockEndPos
							}
						}
						nextBlock := streamCacheBytes[splitPos:] // 下一个块的数据
						streamCache.Truncate(0)
						streamCache.Write(nextBlock)
						lastBlock, lastBlockStartPos, lastBlockEndPos = GetBlockInfo(curPos, r.DingFile.GetBlockSize(), r.DingFile.GetFileSize())
					}
				}
			case <-r.Context.Done():
				zap.S().Warnf("file:%s/%s taskNo:%d ctx done, DoTask exit.", r.OrgRepo, r.FileName, r.TaskNo)
				data.ReportFileProcess(r.Context, 0, 0, consts.StatusDownloadBreak)
				return
			}
		}
	}()
	wg.Wait()
	rawBlock := streamCache.Bytes()
	if curBlock == r.DingFile.getBlockNumber()-1 {
		// 对不足一个block的数据做补全
		if int64(len(rawBlock)) == r.DingFile.GetFileSize()%r.DingFile.GetBlockSize() {
			padding := bytes.Repeat([]byte{0}, int(r.DingFile.GetBlockSize())-len(rawBlock))
			rawBlock = append(rawBlock, padding...)
		}
		lastBlock = curBlock
	}
	// 一个空文件，或文件刚好为blocksize的整数倍，直接标记为完成
	if len(rawBlock) == 0 {
		data.ReportFileProcess(r.Context, lastReportPos, curPos, consts.StatusDownloaded)
	} else if int64(len(rawBlock)) == r.DingFile.GetBlockSize() {
		hasBlockBool, err := r.DingFile.HasBlock(lastBlock)
		if err != nil {
			zap.S().Errorf("HasBlock err.%v", err)
			return
		}
		if !hasBlockBool {
			if err = r.DingFile.WriteBlock(lastBlock, rawBlock); err != nil {
				zap.S().Errorf("last writeBlock err.%v", err)
			}
			zap.S().Debugf("from:%s, %s/%s, taskNo:%d, last block：%d(%d)write done, range：%d-%d.", r.Domain, r.OrgRepo, r.FileName, r.TaskNo, lastBlock, blockNumber, lastBlockStartPos, lastBlockEndPos)
			data.ReportFileProcess(r.Context, lastReportPos, curPos, consts.StatusDownloaded)
		}
	}
	if curPos != rangeEndPos {
		zap.S().Warnf("file:%s, taskNo:%d, remote range (%d) is different from sent size (%d).", r.FileName, r.TaskNo, rangeEndPos-rangeStartPos, curPos-rangeStartPos)
	}
	zap.S().Infof("end remote dotask:%s/%s, taskNo:%d, size:%d, domain:%s, startPos:%d, endPos:%d", r.OrgRepo, r.FileName, r.TaskNo, r.TaskSize, r.Domain, rangeStartPos, rangeEndPos)

}

func (r *RemoteFileTask) OutResult() {
	zap.S().Infof("start remote outResult,taskNo:%d, %s/%s", r.TaskNo, r.OrgRepo, r.FileName)
	for {
		select {
		case chunk, ok := <-r.Queue:
			if !ok {
				zap.S().Debugf("end remote outResult close. taskNo:%d, %s/%s", r.TaskNo, r.OrgRepo, r.FileName)
				return
			}
			select {
			case r.ResponseChan <- chunk:
			case <-r.Context.Done():
				zap.S().Debugf("end remote outResult Context.Done() %s/%s", r.OrgRepo, r.FileName)
				return
			}
		case <-r.Context.Done():
			zap.S().Debugf("end remote outResult fileName:%s/%s,err:%v", r.OrgRepo, r.FileName, r.Context.Err())
			return
		}
	}
}

func (r *RemoteFileTask) GetResponseChan() chan []byte {
	return r.ResponseChan
}

func (r *RemoteFileTask) getFileRangeFromRemote(startPos, endPos int64, contentChan chan<- []byte) error {
	headers := make(map[string]string)
	if r.Authorization != "" {
		headers["authorization"] = r.Authorization
	}
	headers["range"] = fmt.Sprintf("bytes=%d-%d", startPos, endPos-1)
	var (
		rawData                           []byte
		chunkByteLen                      = 0
		attempts                          = 2
		contentEncoding, contentLengthStr = "", ""
		err                               error
		n                                 int
	)
	for i := 0; i < attempts; {
		if _, err = util.RetryRequest(func() (*common.Response, error) {
			err = util.GetStream(r.Domain, r.Uri, headers, func(resp *http.Response) error {
				contentEncoding = resp.Header.Get("content-encoding")
				contentLengthStr = resp.Header.Get("content-length")
				for {
					select {
					case <-r.Context.Done():
						zap.S().Warnf("getFileRangeFromRemote Context.Done err :%s", r.FileName)
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
									return nil
								}
							}
							chunkByteLen += n // 原始数量
						}
						if err != nil {
							if err == io.EOF {
								return nil
							}
							zap.S().Errorf("file:%s, taskNo:%d, statusCode:%d, req remote err.%v", r.FileName, r.TaskNo, resp.StatusCode, err)
							if chunkByteLen > 0 {
								headers["range"] = fmt.Sprintf("bytes=%d-%d", startPos+int64(chunkByteLen), endPos-1)
							}
							return err
						}
					}
				}
			})
			return nil, err
		}); err != nil {
			zap.S().Warnf("GetStream err.%v", err)
			if config.SysConfig.IsCluster() && util.IsInnerDomain(r.Domain) {
				r.Domain = config.SysConfig.GetHFURLBase()
				if chunkByteLen > 0 {
					headers["range"] = fmt.Sprintf("bytes=%d-%d", startPos+int64(chunkByteLen), endPos-1)
				}
				i++
			} else {
				break
			}
		} else {
			break
		}
	}
	if contentEncoding != "" {
		// 这里需要实现解压缩逻辑
		finalData, err := util.DecompressData(rawData, contentEncoding)
		if err != nil {
			zap.S().Errorf("DecompressData err.%v", err)
			return nil
		}
		contentChan <- finalData      // 返回解码后的数据流
		chunkByteLen = len(finalData) // 将解码后的长度复制为原理的chunkBytes
	}
	if contentLengthStr != "" {
		contentLength, err := strconv.Atoi(contentLengthStr) // 原始数据长度
		if err != nil {
			zap.S().Errorf("contentLengthStr conv err.%s", contentLengthStr)
			return nil
		}
		if contentEncoding != "" {
			contentLength = chunkByteLen
		}
		if endPos-startPos != int64(contentLength) {
			zap.S().Errorf("file:%s, taskNo:%d,The content of the response is incomplete. Expected-%d. Accepted-%d", r.FileName, r.TaskNo, endPos-startPos, contentLength)
			return nil
		}
	}
	if endPos-startPos != int64(chunkByteLen) {
		zap.S().Warnf("file:%s, taskNo:%d,The block is incomplete. Expected-%d. Accepted-%d", r.FileName, r.TaskNo, endPos-startPos, chunkByteLen)
		return nil
	}
	return err
}
