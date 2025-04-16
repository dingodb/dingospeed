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
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"sync"

	cache "dingo-hfmirror/internal/data"
	"dingo-hfmirror/pkg/config"
	"dingo-hfmirror/pkg/util"

	"go.uber.org/zap"
)

const (
	CURRENT_OLAH_CACHE_VERSION = 8
	// DEFAULT_BLOCK_MASK_MAX     = 30
	DEFAULT_BLOCK_MASK_MAX = 1024 * 1024

	cost = 1
)

var magicNumber = [4]byte{'O', 'L', 'A', 'H'}

// DingCacheHeader 结构体表示 Olah 缓存文件的头部
type DingCacheHeader struct {
	MagicNumber   [4]byte
	Version       int64
	BlockSize     int64
	FileSize      int64
	BlockMaskSize int64
	BlockNumber   int64
	BlockMask     *Bitset
}

// NewDingCacheHeader 创建一个新的 DingCacheHeader 对象
func NewDingCacheHeader(version, blockSize, fileSize int64) *DingCacheHeader {
	blockNumber := (fileSize + blockSize - 1) / blockSize
	blockMask := NewBitset(int64(DEFAULT_BLOCK_MASK_MAX))
	return &DingCacheHeader{
		MagicNumber:   magicNumber,
		Version:       version,
		BlockSize:     blockSize,
		FileSize:      fileSize,
		BlockMaskSize: DEFAULT_BLOCK_MASK_MAX,
		BlockNumber:   blockNumber,
		BlockMask:     blockMask,
	}
}

// GetHeaderSize 返回头部的大小
func (h *DingCacheHeader) GetHeaderSize() int64 {
	return int64(36 + len(h.BlockMask.bits))
}

// Read 从文件流中读取头部信息
func (h *DingCacheHeader) Read(f *os.File) error {
	magic := make([]byte, 4)
	if _, err := f.Read(magic); err != nil {
		return errors.New("read magic 4 bytes err")
	}
	if !bytes.Equal(magic, []byte{'O', 'L', 'A', 'H'}) {
		return errors.New("file is not a Olah cache file")
	}
	h.MagicNumber = magicNumber
	if err := binary.Read(f, binary.LittleEndian, &h.Version); err != nil {
		return err
	}
	if err := binary.Read(f, binary.LittleEndian, &h.BlockSize); err != nil {
		return err
	}
	if err := binary.Read(f, binary.LittleEndian, &h.FileSize); err != nil {
		return err
	}
	if err := binary.Read(f, binary.LittleEndian, &h.BlockMaskSize); err != nil {
		return err
	}
	h.BlockNumber = (h.FileSize + h.BlockSize - 1) / h.BlockSize
	h.BlockMask = NewBitset(h.BlockMaskSize)
	if _, err := f.Read(h.BlockMask.bits); err != nil {
		return err
	}
	return h.ValidHeader()
}

func (h *DingCacheHeader) ValidHeader() error {
	if h.FileSize > h.BlockMaskSize*h.BlockSize {
		return fmt.Errorf("the size of file %d is out of the max capability of container (%d * %d)", h.FileSize, h.BlockMaskSize, h.BlockSize)
	}
	if h.Version < CURRENT_OLAH_CACHE_VERSION {
		return fmt.Errorf("the Olah Cache file is created by older version Olah. Please remove cache files and retry")
	}
	if h.Version > CURRENT_OLAH_CACHE_VERSION {
		return fmt.Errorf("the Olah Cache file is created by newer version Olah. Please remove cache files and retry")
	}
	return nil
}

// Write 将头部信息写入文件流
func (h *DingCacheHeader) Write(f *os.File) error {
	if _, err := f.Write(h.MagicNumber[:]); err != nil {
		return err
	}
	if err := binary.Write(f, binary.LittleEndian, h.Version); err != nil {
		return err
	}
	if err := binary.Write(f, binary.LittleEndian, h.BlockSize); err != nil {
		return err
	}
	if err := binary.Write(f, binary.LittleEndian, h.FileSize); err != nil {
		return err
	}
	if err := binary.Write(f, binary.LittleEndian, h.BlockMaskSize); err != nil {
		return err
	}
	if _, err := f.Write(h.BlockMask.bits); err != nil {
		return err
	}
	return nil
}

// DingCache 结构体表示 Olah 缓存文件
type DingCache struct {
	path       string
	header     *DingCacheHeader
	isOpen     bool
	headerLock sync.RWMutex
	fileLock   sync.RWMutex
}

// NewDingCache 创建一个新的 DingCache 对象
func NewDingCache(path string, blockSize int64) (*DingCache, error) {
	cache := &DingCache{
		path:       path,
		header:     nil,
		isOpen:     false,
		headerLock: sync.RWMutex{},
	}
	if err := cache.Open(path, blockSize); err != nil {
		return nil, err
	}
	return cache, nil
}

// Open 打开缓存文件
func (c *DingCache) Open(path string, blockSize int64) error {
	if c.isOpen {
		return errors.New("this file has been open")
	}
	if _, err := os.Stat(path); err == nil { // 文件存在
		c.headerLock.Lock()
		defer c.headerLock.Unlock()
		f, err := os.OpenFile(path, os.O_RDONLY, 0644)
		if err != nil {
			return err
		}
		defer f.Close()
		c.header = &DingCacheHeader{}
		if err := c.header.Read(f); err != nil {
			return err
		}
	} else {
		c.headerLock.Lock()
		defer c.headerLock.Unlock()
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0644)
		if err != nil {
			return err
		}
		defer f.Close()
		c.header = NewDingCacheHeader(CURRENT_OLAH_CACHE_VERSION, blockSize, 0)
		if err := c.header.Write(f); err != nil {
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
	if err := c.flushHeader(); err != nil {
		return err
	}
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

// getFileSize 返回文件大小
func (c *DingCache) GetFileSize() int64 {
	c.headerLock.RLock()
	defer c.headerLock.RUnlock()
	return c.header.FileSize
}

// getBlockNumber 返回块数
func (c *DingCache) getBlockNumber() int64 {
	c.headerLock.RLock()
	defer c.headerLock.RUnlock()
	return c.header.BlockNumber
}

// getBlockSize 返回块大小
func (c *DingCache) getBlockSize() int64 {
	c.headerLock.RLock()
	defer c.headerLock.RUnlock()
	return c.header.BlockSize
}

// getHeaderSize 返回头部大小
func (c *DingCache) getHeaderSize() int64 {
	c.headerLock.RLock()
	defer c.headerLock.RUnlock()
	return c.header.GetHeaderSize()
}

func (c *DingCache) resizeHeader(blockNum, fileSize int64) error {
	c.headerLock.RLock()
	defer c.headerLock.RUnlock()
	c.header.BlockNumber = blockNum
	c.header.FileSize = fileSize
	return c.header.ValidHeader()
}

func (c *DingCache) setHeaderBlock(blockIndex int64) error {
	// c.headerLock.Lock()
	// defer c.headerLock.Unlock()
	return c.header.BlockMask.Set(blockIndex)
}

// testHeaderBlock 测试头部块信息
func (c *DingCache) testHeaderBlock(blockIndex int64) (bool, error) {
	c.headerLock.RLock()
	defer c.headerLock.RUnlock()
	return c.header.BlockMask.Test(blockIndex)
}

// padBlock 填充块数据
func (c *DingCache) padBlock(rawBlock []byte) []byte {
	blockSize := c.getBlockSize()
	if int64(len(rawBlock)) < blockSize {
		return append(rawBlock, bytes.Repeat([]byte{0}, int(blockSize)-len(rawBlock))...)
	}
	return rawBlock
}

// HasBlock 检查块是否存在
func (c *DingCache) HasBlock(blockIndex int64) (bool, error) {
	return c.testHeaderBlock(blockIndex)
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
	if blockData, ok := cache.FileBlockCache.Get(key); ok {
		return blockData, nil
	}
	hasBlock, err := c.HasBlock(blockIndex)
	if err != nil {
		return nil, err
	}
	if !hasBlock {
		return nil, nil
	}
	offset := c.getHeaderSize() + blockIndex*c.getBlockSize()
	f, err := os.OpenFile(c.path, os.O_RDONLY, 0644)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := f.Seek(offset, 0); err != nil {
		return nil, err
	}
	rawBlock := make([]byte, c.getBlockSize()) // 读取当前块（blockIndex）的数据
	if _, err := f.Read(rawBlock); err != nil {
		return nil, err
	}
	c.readBlockAndCache(f, blockIndex)
	block := c.padBlock(rawBlock)
	return block, nil
}

func (c *DingCache) readBlockAndCache(f *os.File, blockIndex int64) {
	var cacheFlag bool
	memoryUsedPercent := config.SysInfo.MemoryUsedPercent
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
			prefetchRawBlock := make([]byte, c.getBlockSize())
			if _, err := f.Read(prefetchRawBlock); err != nil {
				zap.S().Errorf("read err. newOffsetBlock:%d, %v", newOffsetBlock, err)
				break
			}
			blockByte := c.padBlock(prefetchRawBlock)
			cache.FileBlockCache.SetWithTTL(key, blockByte, cost, config.SysConfig.GetPrefetchBlockTTL())
			// 删除上一个缓存周期的内容，释放内存
			oldOffsetBlock := newOffsetBlock - config.SysConfig.GetPrefetchBlocks() - 1
			if oldOffsetBlock > 0 {
				oldKey := c.getBlockKey(newOffsetBlock)
				cache.FileBlockCache.Del(oldKey)
			}
			cacheFlag = true
		} else {
			break
		}
	}
	if cacheFlag {
		cache.FileBlockCache.Wait()
	}
}

func (c *DingCache) WriteBlock(blockIndex int64, blockBytes []byte) error {
	if !c.isOpen {
		return errors.New("this file has been closed")
	}
	if blockIndex >= c.getBlockNumber() {
		return errors.New("invalid block index")
	}
	if int64(len(blockBytes)) != c.getBlockSize() {
		return errors.New("block size does not match the cache's block size")
	}
	offset := c.getHeaderSize() + blockIndex*c.getBlockSize()
	f, err := os.OpenFile(c.path, os.O_RDWR, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Seek(offset, 0); err != nil {
		return err
	}
	if (blockIndex+1)*c.getBlockSize() > c.GetFileSize() {
		realBlockBytes := blockBytes[:c.GetFileSize()-blockIndex*c.getBlockSize()]
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

// resizeFileSize 调整文件大小
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

// Resize 调整缓存大小
func (c *DingCache) Resize(fileSize int64) error {
	if !c.isOpen {
		return errors.New("this file has been closed")
	}
	bs := c.getBlockSize()
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

func (c *DingCache) getBlockKey(blockIndex int64) string {
	simpleKey := fmt.Sprintf("%s/%d", c.path, blockIndex)
	return util.Md5(simpleKey)
}
