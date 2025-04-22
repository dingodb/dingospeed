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

	"go.uber.org/zap"
)

type DownloadTask struct {
	TaskNo        int
	RangeStartPos int64
	RangeEndPos   int64
	TaskSize      int
	FileName      string
	DingFile      *DingCache      `json:"-"`
	ResponseChan  chan []byte     `json:"-"`
	Context       context.Context `json:"-"`
}

type CacheFileTask struct {
	DownloadTask
}

func NewCacheFileTask(taskNo int, rangeStartPos int64, rangeEndPos int64) *CacheFileTask {
	c := &CacheFileTask{}
	c.TaskNo = taskNo
	c.RangeStartPos = rangeStartPos
	c.RangeEndPos = rangeEndPos
	return c
}

func (c CacheFileTask) DoTask() {
}

func (c CacheFileTask) OutResult() {
	startBlock := c.RangeStartPos / c.DingFile.getBlockSize()
	endBlock := (c.RangeEndPos - 1) / c.DingFile.getBlockSize()
	curPos := c.RangeStartPos
	for curBlock := startBlock; curBlock <= endBlock; curBlock++ {
		if c.Context.Err() != nil {
			zap.S().Warnf("cache OutResult file:%s", c.FileName)
			return
		}
		_, blockStartPos, blockEndPos := getBlockInfo(curPos, c.DingFile.getBlockSize(), c.DingFile.GetFileSize())
		hasBlockBool, err := c.DingFile.HasBlock(curBlock)
		if err != nil {
			zap.S().Errorf("HasBlock err. file:%s, curBlock:%d, curPos:%d, %v", c.FileName, curBlock, curPos, err)
			continue
		}
		if !hasBlockBool {
			zap.S().Errorf("block not exist. file:%s, curBlock:%d,curPos:%d", c.FileName, curBlock, curPos)
			break
		}
		rawBlock, err := c.DingFile.ReadBlock(curBlock)
		if err != nil {
			zap.S().Errorf("ReadBlock err file:%s, %v", c.FileName, err)
			continue
		}
		sPos := max(c.RangeStartPos, blockStartPos) - blockStartPos
		ePos := min(c.RangeEndPos, blockEndPos) - blockStartPos
		rawLen := int64(len(rawBlock))
		if rawLen == 0 || sPos > rawLen {
			zap.S().Errorf("read rawBlock err,%s, rawLen:%d, sPos:%d,ePos:%d, %v", c.FileName, rawLen, sPos, ePos, err)
			continue
		}
		if ePos > rawLen {
			zap.S().Warnf("block incomplete,%s, rawLen:%d, sPos:%d,ePos:%d, %v", c.FileName, rawLen, sPos, ePos, err)
			ePos = rawLen
		}
		chunk := rawBlock[sPos:ePos]
		c.ResponseChan <- chunk
		curPos += int64(len(chunk))
	}
	if curPos != c.RangeEndPos {
		zap.S().Errorf("file:%s, cache range from %d to %d is incomplete.", c.FileName, c.RangeStartPos, c.RangeEndPos)
	}
	zap.S().Infof("cache file out:%s, taskNo:%d, size:%d, startPos:%d, endPos:%d", c.FileName, c.TaskNo, c.TaskSize, c.RangeStartPos, c.RangeEndPos)
}

func (c CacheFileTask) GetResponseChan() chan []byte {
	return c.ResponseChan
}
