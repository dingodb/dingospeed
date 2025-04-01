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
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"

	"dingo-hfmirror/pkg/config"
	"dingo-hfmirror/pkg/consts"
	"dingo-hfmirror/pkg/util"

	"go.uber.org/zap"
)

func FileDownload(hfUrl, savePath string, fileSize int64, reqHeaders map[string]string, ret chan []byte) {
	defer close(ret) // 所有数据处理完成，需关闭channel
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

	if len(reqHeaders) > 0 {
		zap.S().Debugf("reqHeaders data:%v", reqHeaders)
	}

	var headRange = reqHeaders["range"]
	if headRange == "" {
		headRange = fmt.Sprintf("bytes=%d-%d", 0, fileSize-1)
	}
	startPos, endPos := parseRangeParams(headRange, fileSize)
	endPos++
	rangesAndCacheList := getContiguousRanges(dingFile, startPos, endPos)
	zap.S().Debugf("rangesAndCacheList:%v", util.ToJsonString(rangesAndCacheList))
	for _, rangeInfo := range rangesAndCacheList {
		contentChan := make(chan []byte, consts.RespChanSize)
		rangeStartPos, rangeEndPos := rangeInfo.StartPos, rangeInfo.EndPos
		zap.S().Debugf("file:%s, startPos:%d, endPos:%d, isRemote:%v", savePath, rangeStartPos, rangeEndPos, rangeInfo.IsRemote)
		if rangeInfo.IsRemote {
			go GetFileRangeFromRemote(reqHeaders["authorization"], hfUrl, rangeStartPos, rangeEndPos, contentChan)
		} else {
			go GetFileRangeFromCache(dingFile, rangeStartPos, rangeEndPos, contentChan)
		}
		curPos := rangeStartPos
		streamCache := bytes.Buffer{}
		lastBlock, lastBlockStartPos, lastBlockEndPos := getBlockInfo(curPos, dingFile.getBlockSize(), dingFile.GetFileSize())
		var curBlock int64
		var wg sync.WaitGroup
		wg.Add(1)
		go func(isRemote bool) {
			defer func() {
				wg.Done()
			}()
			for {
				select {
				case chunk, ok := <-contentChan:
					{
						if !ok {
							zap.S().Debugf("contentChan is close.")
							return
						}
						ret <- chunk
						curPos += int64(len(chunk))
						if !isRemote { // 若为缓存的数据块，则无需下面的数据块比较和写入
							continue
						}
						if len(chunk) != 0 {
							streamCache.Write(chunk)
						}
						curBlock = curPos / dingFile.getBlockSize()
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
							if int64(len(rawBlock)) == dingFile.getBlockSize() {
								hasBlockBool, err := dingFile.HasBlock(lastBlock)
								if err != nil {
									zap.S().Errorf("HasBlock err.%v", err)
								}
								if err == nil && !hasBlockBool {
									if err = dingFile.WriteBlock(lastBlock, rawBlock); err != nil {
										zap.S().Errorf("writeBlock err.%v", err)
									}
									zap.S().Debugf("mid block：%d write complete, len：%d.", lastBlock, len(rawBlock))
								}
							}
							lastBlock, lastBlockStartPos, lastBlockEndPos = getBlockInfo(curPos, dingFile.getBlockSize(), dingFile.GetFileSize())
						}
					}

				}
			}
		}(rangeInfo.IsRemote)
		wg.Wait()
		if rangeInfo.IsRemote {
			rawBlock := streamCache.Bytes()
			if curBlock == dingFile.getBlockNumber()-1 {
				// 对不足一个block的数据做补全
				if int64(len(rawBlock)) == dingFile.GetFileSize()%dingFile.getBlockSize() {
					padding := bytes.Repeat([]byte{0}, int(dingFile.getBlockSize())-len(rawBlock))
					rawBlock = append(rawBlock, padding...)
				}
				lastBlock = curBlock
			}
			if int64(len(rawBlock)) == dingFile.getBlockSize() {
				hasBlockBool, err := dingFile.HasBlock(lastBlock)
				if err != nil {
					zap.S().Errorf("HasBlock err.%v", err)
					return
				}
				if !hasBlockBool {
					if err = dingFile.WriteBlock(lastBlock, rawBlock); err != nil {
						zap.S().Errorf("last writeBlock err.%v", err)
					}
					zap.S().Debugf("last block：%d write complete, len：%d.", lastBlock, len(rawBlock))
				}
			}
		}
		if curPos != rangeEndPos {
			if rangeInfo.IsRemote {
				zap.S().Errorf("The size of remote range (%d) is different from sent size (%d).", rangeEndPos-rangeStartPos, curPos-rangeStartPos)
			} else {
				zap.S().Errorf("The size of cached range (%d) is different from sent size (%d).", rangeEndPos-rangeStartPos, curPos-rangeStartPos)
			}
		}
		zap.S().Debugf("range %d-%d complete.", rangeInfo.StartPos, rangeInfo.EndPos)
	}

}

func GetFileRangeFromCache(dingFile *DingCache, rangeStartPos, rangeEndPos int64, contentChan chan<- []byte) {
	startBlock := rangeStartPos / dingFile.getBlockSize()
	endBlock := (rangeEndPos - 1) / dingFile.getBlockSize()
	curPos := rangeStartPos
	defer close(contentChan)
	for curBlock := startBlock; curBlock <= endBlock; curBlock++ {
		_, blockStartPos, blockEndPos := getBlockInfo(curPos, dingFile.getBlockSize(), dingFile.GetFileSize())
		hasBlockBool, err := dingFile.HasBlock(curBlock)
		if err != nil {
			zap.S().Errorf("HasBlock err. curBlock:%d,curPos:%d, %v", curBlock, curPos, err)
			return
		}
		if !hasBlockBool {
			zap.S().Errorf("read block which has not been cached.curBlock:%d, ")
			return
		}
		rawBlock, err := dingFile.ReadBlock(curBlock)
		if err != nil {
			zap.S().Errorf("dingFile.ReadBlock err.%v", err)
			return
		}
		chunk := rawBlock[max(rangeStartPos, blockStartPos)-blockStartPos : min(rangeEndPos, blockEndPos)-blockStartPos]
		contentChan <- chunk
		curPos += int64(len(chunk))
	}
	if curPos != rangeEndPos {
		zap.S().Errorf("The cache range from %d to %d is incomplete.", rangeStartPos, rangeEndPos)
	}
}

func GetFileRangeFromRemote(authorization, url string, startPos, endPos int64, contentChan chan<- []byte) {
	client := &http.Client{}
	client.Timeout = config.SysConfig.GetReqTimeOut()
	headers := make(map[string]string)
	if authorization != "" {
		headers["authorization"] = authorization
	}
	headers["range"] = fmt.Sprintf("bytes=%d-%d", startPos, endPos-1)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		zap.S().Errorf("new request err.%v", err)
		return
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		zap.S().Errorf("do request err.%v", err)
		return
	}
	var rawData []byte
	chunkByteLen := 0
	var contentEncoding = ""
	defer func() {
		zap.S().Debugf("GetFileRangeFromRemote exit. %s-%d", url, chunkByteLen)
		defer resp.Body.Close()
		defer close(contentChan)
	}()
	contentEncoding = resp.Header.Get("content-encoding")
	for {
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
				break
			}
			zap.S().Errorf("req remote err.%v", err)
			return
		}
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
	if contentLengthStr := resp.Header.Get("content-length"); contentLengthStr != "" {
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

// get_block_info 函数
func getBlockInfo(pos, blockSize, fileSize int64) (int64, int64, int64) {
	curBlock := pos / blockSize
	blockStartPos := curBlock * blockSize
	blockEndPos := min((curBlock+1)*blockSize, fileSize)
	return curBlock, blockStartPos, blockEndPos
}

// 获取
func getContiguousRanges(dingFile *DingCache, startPos, endPos int64) []*RangeInfo {
	startBlock := startPos / dingFile.getBlockSize()
	endBlock := (endPos - 1) / dingFile.getBlockSize()

	rangeStartPos, curPos := startPos, startPos
	hasBlockBool, err := dingFile.HasBlock(startBlock)
	if err != nil {
		zap.S().Errorf("%v", err)
		return nil
	}
	rangeIsRemote := !hasBlockBool // 不存在，从远程获取，为true
	var rangesAndCacheList []*RangeInfo
	for curBlock := startBlock; curBlock <= endBlock; curBlock++ {
		_, _, blockEndPos := getBlockInfo(curPos, dingFile.getBlockSize(), dingFile.GetFileSize())
		hasBlockBool, err = dingFile.HasBlock(curBlock)
		if err != nil {
			zap.S().Errorf("HasBlock err. curBlock:%d,curPos:%d, %v", curBlock, curPos, err)
			return nil
		}
		curIsRemote := !hasBlockBool // 不存在，从远程获取，为true，存在为false。
		if rangeIsRemote != curIsRemote {
			if rangeStartPos < curPos {
				rangesAndCacheList = append(rangesAndCacheList, &RangeInfo{StartPos: rangeStartPos, EndPos: curPos, IsRemote: rangeIsRemote})
			}
			rangeStartPos = curPos
			rangeIsRemote = curIsRemote
		}
		curPos = blockEndPos
	}
	rangesAndCacheList = append(rangesAndCacheList, &RangeInfo{StartPos: rangeStartPos, EndPos: endPos, IsRemote: rangeIsRemote})
	return rangesAndCacheList
}

type RangeInfo struct {
	StartPos int64
	EndPos   int64
	IsRemote bool
}
