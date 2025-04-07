package downloader

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"

	"dingo-hfmirror/pkg/config"
	"dingo-hfmirror/pkg/consts"
	"dingo-hfmirror/pkg/util"

	"go.uber.org/zap"
)

type RemoteFileTask struct {
	DownloadTask
	Context       context.Context `json:"-"`
	authorization string
	hfUrl         string
	Queue         chan []byte `json:"-"`
}

func NewRemoteFileTask(taskNo int, rangeStartPos int64, rangeEndPos int64) *RemoteFileTask {
	c := &RemoteFileTask{}
	c.TaskNo = taskNo
	c.RangeStartPos = rangeStartPos
	c.RangeEndPos = rangeEndPos
	return c
}

// 分段下载
func (r RemoteFileTask) DoTask() {
	contentChan := make(chan []byte, consts.RespChanSize)
	rangeStartPos, rangeEndPos := r.RangeStartPos, r.RangeEndPos
	zap.S().Debugf("remote file download:%s, taskNo:%d, startPos:%d, endPos:%d", r.hfUrl, r.TaskNo, rangeStartPos, rangeEndPos)
	go getFileRangeFromRemote(r.Context, r.authorization, r.hfUrl, rangeStartPos, rangeEndPos, contentChan)
	curPos := rangeStartPos
	streamCache := bytes.Buffer{}
	lastBlock, lastBlockStartPos, lastBlockEndPos := getBlockInfo(curPos, r.DingFile.getBlockSize(), r.DingFile.GetFileSize()) // 块编号，开始位置，结束位置
	var curBlock int64

	defer close(r.Queue)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer func() {
			wg.Done()
		}()
		for {
			select {
			case chunk, ok := <-contentChan:
				{
					if !ok {
						return
					}
					r.Queue <- chunk // 先写到缓存
					curPos += int64(len(chunk))
					if len(chunk) != 0 {
						streamCache.Write(chunk)
					}
					curBlock = curPos / r.DingFile.getBlockSize()
					// 若是一个新的数据块，则将上一个数据块持久化。
					if curBlock != lastBlock {
						splitPos := lastBlockEndPos - max(lastBlockStartPos, rangeStartPos)
						streamCacheBytes := streamCache.Bytes()
						if splitPos > int64(len(streamCacheBytes)) {
							// 正常不会出现splitPos>len(streamCacheBytes),若出现只能降级处理。
							zap.S().Errorf("splitPos err.%d-%d", splitPos, len(streamCacheBytes))
							splitPos = int64(len(streamCacheBytes))
						}
						rawBlock := streamCacheBytes[:splitPos]  // 当前块的数据
						nextBlock := streamCacheBytes[splitPos:] // 下一个块的数据
						streamCache.Truncate(0)
						streamCache.Write(nextBlock)
						if int64(len(rawBlock)) == r.DingFile.getBlockSize() {
							hasBlockBool, err := r.DingFile.HasBlock(lastBlock)
							if err != nil {
								zap.S().Errorf("HasBlock err.%v", err)
							}
							if err == nil && !hasBlockBool {
								if err = r.DingFile.WriteBlock(lastBlock, rawBlock); err != nil {
									zap.S().Errorf("writeBlock err.%v", err)
								}
								zap.S().Debugf("taskNo:%d, block：%d write done, range：%d-%d.", r.TaskNo, lastBlock, lastBlockStartPos, lastBlockEndPos)
							}
						}
						lastBlock, lastBlockStartPos, lastBlockEndPos = getBlockInfo(curPos, r.DingFile.getBlockSize(), r.DingFile.GetFileSize())
					}
				}
			case <-r.Context.Done():
				return
			}
		}
	}()
	wg.Wait()
	rawBlock := streamCache.Bytes()
	if curBlock == r.DingFile.getBlockNumber()-1 {
		// 对不足一个block的数据做补全
		if int64(len(rawBlock)) == r.DingFile.GetFileSize()%r.DingFile.getBlockSize() {
			padding := bytes.Repeat([]byte{0}, int(r.DingFile.getBlockSize())-len(rawBlock))
			rawBlock = append(rawBlock, padding...)
		}
		lastBlock = curBlock
	}
	if int64(len(rawBlock)) == r.DingFile.getBlockSize() {
		hasBlockBool, err := r.DingFile.HasBlock(lastBlock)
		if err != nil {
			zap.S().Errorf("HasBlock err.%v", err)
			return
		}
		if !hasBlockBool {
			if err = r.DingFile.WriteBlock(lastBlock, rawBlock); err != nil {
				zap.S().Errorf("last writeBlock err.%v", err)
			}
			zap.S().Debugf("taskNo:%d, last block：%d write done, range：%d-%d.", r.TaskNo, lastBlock, lastBlockStartPos, lastBlockEndPos)
		}
	}
	if curPos != rangeEndPos {
		zap.S().Errorf("taskNo:%d, remote range (%d) is different from sent size (%d).", r.TaskNo, rangeEndPos-rangeStartPos, curPos-rangeStartPos)
	}
}

func (r RemoteFileTask) OutResult() {
	for data := range r.Queue {
		r.ResponseChan <- data
	}
}

func getFileRangeFromRemote(ctx context.Context, authorization, url string, startPos, endPos int64, contentChan chan<- []byte) {
	headers := make(map[string]string)
	if authorization != "" {
		headers["authorization"] = authorization
	}
	headers["range"] = fmt.Sprintf("bytes=%d-%d", startPos, endPos-1)

	var rawData []byte
	chunkByteLen := 0
	var contentEncoding, contentLengthStr = "", ""
	defer func() {
		defer close(contentChan)
	}()

	if err := util.GetStream(url, headers, config.SysConfig.GetReqTimeOut(), func(resp *http.Response) {
		contentEncoding = resp.Header.Get("content-encoding")
		contentLengthStr = resp.Header.Get("content-length")
		for {
			select {
			case <-ctx.Done():
				zap.S().Infof("ctx done, getFileRangeFromRemote exit.")
				return
			default:
				chunk := make([]byte, config.SysConfig.Download.RespChunkSize)
				n, err := resp.Body.Read(chunk)
				if n > 0 {
					if contentEncoding != "" { // 数据有编码，先收集，后面解码
						rawData = append(rawData, chunk[:n]...)
					} else {
						contentChan <- chunk[:n] // 没有编码，可以直接返回原始数据
					}
					chunkByteLen += n // 原始数量
				}
				if err != nil {
					if err == io.EOF {
						return
					}
					zap.S().Errorf("req remote err.%v", err)
					return
				}
			}
		}
	}); err != nil {
		zap.S().Errorf("GetStream err.%v", err)
		return
	}
	if contentEncoding != "" {
		// 这里需要实现解压缩逻辑
		finalData, err := util.DecompressData(rawData, contentEncoding)
		if err != nil {
			zap.S().Errorf("DecompressData err.%v", err)
			return
		}
		contentChan <- finalData      // 返回解码后的数据流
		chunkByteLen = len(finalData) // 将解码后的长度复制为原理的chunkBytes
	}
	if contentLengthStr != "" {
		contentLength, err := strconv.Atoi(contentLengthStr) // 原始数据长度
		if err != nil {
			zap.S().Errorf("contentLengthStr conv err.%s", contentLengthStr)
			return
		}
		if contentEncoding != "" {
			contentLength = chunkByteLen
		}
		if endPos-startPos != int64(contentLength) {
			zap.S().Errorf("The content of the response is incomplete. Expected-%d. Accepted-%d", endPos-startPos, contentLength)
			return
		}
	}
	if endPos-startPos != int64(chunkByteLen) {
		zap.S().Errorf("The block is incomplete. Expected-%d. Accepted-%d", endPos-startPos, chunkByteLen)
		return
	}
}
