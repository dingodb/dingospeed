package downloader

import (
	"go.uber.org/zap"
)

type DownloadTask struct {
	TaskNo        int
	RangeStartPos int64
	RangeEndPos   int64
	DingFile      *DingCache  `json:"-"`
	ResponseChan  chan []byte `json:"-"`
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
		_, blockStartPos, blockEndPos := getBlockInfo(curPos, c.DingFile.getBlockSize(), c.DingFile.GetFileSize())
		hasBlockBool, err := c.DingFile.HasBlock(curBlock)
		if err != nil {
			zap.S().Errorf("HasBlock err. curBlock:%d,curPos:%d, %v", curBlock, curPos, err)
			return
		}
		if !hasBlockBool {
			zap.S().Debugf("block not exist. curBlock:%d,curPos:%d", curBlock, curPos)
			return
		}
		rawBlock, err := c.DingFile.ReadBlock(curBlock)
		if err != nil {
			zap.S().Errorf("c.DingFile.ReadBlock err.%v", err)
			return
		}
		chunk := rawBlock[max(c.RangeStartPos, blockStartPos)-blockStartPos : min(c.RangeEndPos, blockEndPos)-blockStartPos]
		c.ResponseChan <- chunk
		curPos += int64(len(chunk))
	}
	if curPos != c.RangeEndPos {
		zap.S().Errorf("The cache range from %d to %d is incomplete.", c.RangeStartPos, c.RangeEndPos)
	}
}
