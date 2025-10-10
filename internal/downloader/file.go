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
	"errors"
	"fmt"
	"os"
	"sync"

	cache "dingospeed/internal/data"
	"dingospeed/pkg/config"
	"dingospeed/pkg/util"

	"go.uber.org/zap"
)

const (
	CURRENT_OLAH_CACHE_VERSION = 8
	// DEFAULT_BLOCK_MASK_MAX     = 30
	DEFAULT_BLOCK_MASK_MAX uint64 = 1024 * 1024

	cost = 1
)

// DingCache 结构体表示 Olah 缓存文件
type DingCache struct {
	path       string
	header     *DingCacheHeader
	isOpen     bool
	headerLock sync.RWMutex
	fileLock   sync.RWMutex
}

func NewDingCache(path string, blockSize int64) (*DingCache, error) {
	dingCache := &DingCache{
		path:       path,
		header:     nil,
		isOpen:     false,
		headerLock: sync.RWMutex{},
	}
	if err := dingCache.Open(path, blockSize); err != nil {
		return nil, err
	}
	return dingCache, nil
}

func (c *DingCache) Open(path string, blockSize int64) error {
	if c.isOpen {
		return errors.New("this file has been open")
	}
	var f *os.File
	if _, err := os.Stat(path); err == nil { // 文件存在
		c.headerLock.Lock()
		defer c.headerLock.Unlock()
		f, err = os.OpenFile(path, os.O_RDWR, 0644) // null file
		if err != nil {
			return err
		}
		defer f.Close()
		c.header = &DingCacheHeader{}
		if err = c.header.Read(f); err != nil {
			c.header = NewDingCacheHeader(CURRENT_OLAH_CACHE_VERSION, uint64(blockSize), 0)
			if err = c.header.Write(f); err != nil {
				return err
			}
		}
	} else {
		c.headerLock.Lock()
		defer c.headerLock.Unlock()
		f, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0644)
		if err != nil {
			return err
		}
		defer f.Close()
		c.header = NewDingCacheHeader(CURRENT_OLAH_CACHE_VERSION, uint64(blockSize), 0)
		if err = c.header.Write(f); err != nil {
			return err
		}
	}
	c.isOpen = true
	return nil
}

// Close 关闭缓存文件
func (c *DingCache) Close() error {
	if !c.isOpen {
		return errors.New("This file has been close.")
	}
	c.fileLock.Lock()
	defer c.fileLock.Unlock()
	// 20250619 fix when the file is read, the update date is modified
	// if err := c.flushHeader(); err != nil {
	// 	return err
	// }
	c.path = ""
	c.header = nil
	c.isOpen = false
	return nil
}

func (c *DingCache) flushHeader() error {
	// c.headerLock.Lock()
	// defer c.headerLock.Unlock()
	f, err := os.OpenFile(c.path, os.O_RDWR, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Seek(0, 0); err != nil {
		return err
	}
	return c.header.Write(f)
}

func (c *DingCache) GetPath() string {
	return c.path
}

// getFileSize 返回文件大小
func (c *DingCache) GetFileSize() int64 {
	c.headerLock.RLock()
	defer c.headerLock.RUnlock()
	return int64(c.header.FileSize)
}

// getBlockNumber 返回块数
func (c *DingCache) getBlockNumber() int64 {
	c.headerLock.RLock()
	defer c.headerLock.RUnlock()
	return int64(c.header.BlockNumber)
}

// GetBlockSize 返回块大小
func (c *DingCache) GetBlockSize() int64 {
	c.headerLock.RLock()
	defer c.headerLock.RUnlock()
	return int64(c.header.BlockSize)
}

// getHeaderSize 返回头部大小
func (c *DingCache) getHeaderSize() int64 {
	c.headerLock.RLock()
	defer c.headerLock.RUnlock()
	return c.header.GetHeaderSize()
}

func (c *DingCache) setHeaderBlock(blockIndex int64) error {
	// c.headerLock.Lock()
	// defer c.headerLock.Unlock()
	return c.header.BlockMask.Set(uint64(blockIndex))
}

func (c *DingCache) HasBlock(blockIndex int64) (bool, error) {
	c.headerLock.RLock()
	defer c.headerLock.RUnlock()
	return c.header.BlockMask.Test(uint64(blockIndex))
}

// ReadBlock 读取块数据，该操作会预先缓存prefetchBlocks个块数据，避免每次读取时对文件做操作。

func (c *DingCache) ReadBlock(blockIndex int64) ([]byte, error) {
	if !c.isOpen {
		return nil, errors.New("this file has been closed")
	}
	if blockIndex >= c.getBlockNumber() {
		return nil, errors.New("Invalid block index.")
	}
	key := c.getBlockKey(blockIndex)
	if config.SysConfig.EnableReadBlockCache() {
		if blockData, ok := cache.FileBlockCache.Get(key); ok {
			// 若当前为最后一个块，则移除最近的16个块
			if blockIndex == c.getBlockNumber()-1 {
				for i := config.SysConfig.GetPrefetchBlocks(); i >= 0; i-- {
					no := blockIndex - i
					if no >= 0 {
						oldKey := c.getBlockKey(no)
						cache.FileBlockCache.Delete(oldKey)
					}
				}
			}
			return blockData.([]byte), nil
		}
	}
	hasBlock, err := c.HasBlock(blockIndex)
	if err != nil {
		return nil, err
	}
	if !hasBlock {
		return nil, nil
	}
	offset := c.getHeaderSize() + blockIndex*c.GetBlockSize()
	f, err := os.OpenFile(c.path, os.O_RDONLY, 0644)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := f.Seek(offset, 0); err != nil {
		return nil, err
	}
	rawBlock := make([]byte, c.GetBlockSize()) // 读取当前块（blockIndex）的数据
	if _, err := f.Read(rawBlock); err != nil {
		return nil, err
	}
	if config.SysConfig.EnableReadBlockCache() {
		c.readBlockAndCache(f, blockIndex)
	}
	block := c.padBlock(rawBlock)
	return block, nil
}

func (c *DingCache) readBlockAndCache(f *os.File, blockIndex int64) {
	memoryUsedPercent := config.SystemInfo.MemoryUsedPercent
	if memoryUsedPercent != 0 && memoryUsedPercent >= config.SysConfig.GetPrefetchMemoryUsedThreshold() {
		return
	}
	// 读取并缓存当前块后的16个块
	for blockOffset := int64(1); blockOffset <= config.SysConfig.GetPrefetchBlocks(); blockOffset++ {
		newOffsetBlock := blockIndex + blockOffset
		if newOffsetBlock >= c.getBlockNumber() {
			break
		}
		hasNextBlock, err := c.HasBlock(newOffsetBlock)
		if err != nil {
			zap.S().Errorf("hasBlock err. newOffsetBlock:%d, %v", newOffsetBlock, err)
			break
		}
		if hasNextBlock {
			key := c.getBlockKey(newOffsetBlock)
			prefetchRawBlock := make([]byte, c.GetBlockSize())
			if _, err := f.Read(prefetchRawBlock); err != nil {
				zap.S().Errorf("read err. newOffsetBlock:%d, %v", newOffsetBlock, err)
				break
			}
			blockByte := c.padBlock(prefetchRawBlock)
			cache.FileBlockCache.Set(key, blockByte)
			// 删除上一个缓存周期的内容，释放内存
			oldOffsetBlock := newOffsetBlock - config.SysConfig.GetPrefetchBlocks() - 1
			if oldOffsetBlock > 0 {
				oldKey := c.getBlockKey(newOffsetBlock)
				cache.FileBlockCache.Delete(oldKey)
			}
		} else {
			break
		}
	}
}

// padBlock 填充块数据
func (c *DingCache) padBlock(rawBlock []byte) []byte {
	blockSize := c.GetBlockSize()
	if int64(len(rawBlock)) < blockSize {
		return append(rawBlock, bytes.Repeat([]byte{0}, int(blockSize)-len(rawBlock))...)
	}
	return rawBlock
}

func (c *DingCache) WriteBlock(blockIndex int64, blockBytes []byte) error {
	if !c.isOpen {
		return errors.New("this file has been closed")
	}
	if blockIndex >= c.getBlockNumber() {
		return errors.New("invalid block index")
	}
	if int64(len(blockBytes)) != c.GetBlockSize() {
		return errors.New("block size does not match the cache's block size")
	}
	offset := c.getHeaderSize() + blockIndex*c.GetBlockSize()
	f, err := os.OpenFile(c.path, os.O_RDWR, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Seek(offset, 0); err != nil {
		return err
	}
	if (blockIndex+1)*c.GetBlockSize() > c.GetFileSize() {
		realBlockBytes := blockBytes[:c.GetFileSize()-blockIndex*c.GetBlockSize()]
		if _, err = f.Write(realBlockBytes); err != nil {
			return err
		}
	} else {
		if _, err = f.Write(blockBytes); err != nil {
			return err
		}
	}
	c.fileLock.Lock()
	defer c.fileLock.Unlock()
	if err = c.setHeaderBlock(blockIndex); err != nil {
		return err
	}
	if err = c.flushHeader(); err != nil {
		return err
	}
	// key := c.getBlockKey(blockIndex)  不需要删除，本来就没有
	// cache.FileBlockCache.Del(key)
	return nil
}

func (c *DingCache) Resize(fileSize int64) error {
	if !c.isOpen {
		return errors.New("this file has been closed")
	}
	bs := c.GetBlockSize()
	newBlockNum := (fileSize + bs - 1) / bs
	c.fileLock.Lock()
	defer c.fileLock.Unlock()
	if err := c.resizeFileSize(fileSize); err != nil {
		return err
	}
	// 设置块数量、文件大小参数
	if err := c.resizeHeader(newBlockNum, fileSize); err != nil {
		return err
	}
	return c.flushHeader()
}

func (c *DingCache) resizeFileSize(fileSize int64) error {
	if !c.isOpen {
		return errors.New("this file has been closed")
	}
	if fileSize == c.GetFileSize() {
		return nil
	}

	if fileSize < c.GetFileSize() {
		return errors.New("invalid resize file size. New file size must be greater than the current file size")
	}
	f, err := os.OpenFile(c.path, os.O_RDWR, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	newBinSize := c.getHeaderSize() + fileSize
	if _, err = f.Seek(newBinSize-1, 0); err != nil {
		return err
	}
	if _, err = f.Write([]byte{0}); err != nil {
		return err
	}
	if err = f.Truncate(newBinSize); err != nil {
		return err
	}
	return nil
}

func (c *DingCache) resizeHeader(blockNum, fileSize int64) error {
	c.headerLock.RLock()
	defer c.headerLock.RUnlock()
	c.header.BlockNumber = uint64(blockNum)
	c.header.FileSize = uint64(fileSize)
	return c.header.ValidHeader()
}

func (c *DingCache) getBlockKey(blockIndex int64) string {
	simpleKey := fmt.Sprintf("%s/%d", c.path, blockIndex)
	return util.Md5(simpleKey)
}

// get_block_info 函数
func GetBlockInfo(pos, blockSize, fileSize int64) (int64, int64, int64) {
	curBlock := pos / blockSize
	blockStartPos := curBlock * blockSize
	blockEndPos := min((curBlock+1)*blockSize, fileSize)
	return curBlock, blockStartPos, blockEndPos
}
