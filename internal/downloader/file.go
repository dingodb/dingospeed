package downloader

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"sync"
)

const (
	CURRENT_OLAH_CACHE_VERSION = 8
	DEFAULT_BLOCK_MASK_MAX     = 1024 * 1024
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
	path            string
	header          *DingCacheHeader
	isOpen          bool
	headerLock      sync.Mutex
	blocksReadCache map[int64][]byte
	prefetchBlocks  int64
}

// NewDingCache 创建一个新的 DingCache 对象
func NewDingCache(path string, blockSize int64) (*DingCache, error) {
	cache := &DingCache{
		path:            path,
		header:          nil,
		isOpen:          false,
		headerLock:      sync.Mutex{},
		blocksReadCache: make(map[int64][]byte),
		prefetchBlocks:  16,
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
	if err := c.flushHeader(); err != nil {
		return err
	}
	c.path = ""
	c.header = nil
	c.blocksReadCache = make(map[int64][]byte)
	c.isOpen = false
	return nil
}

// flushHeader 刷新头部信息到文件
func (c *DingCache) flushHeader() error {
	c.headerLock.Lock()
	defer c.headerLock.Unlock()
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
	c.headerLock.Lock()
	defer c.headerLock.Unlock()
	return c.header.FileSize
}

// getBlockNumber 返回块数
func (c *DingCache) getBlockNumber() int64 {
	c.headerLock.Lock()
	defer c.headerLock.Unlock()
	return c.header.BlockNumber
}

// getBlockSize 返回块大小
func (c *DingCache) getBlockSize() int64 {
	c.headerLock.Lock()
	defer c.headerLock.Unlock()
	return c.header.BlockSize
}

// getHeaderSize 返回头部大小
func (c *DingCache) getHeaderSize() int64 {
	c.headerLock.Lock()
	defer c.headerLock.Unlock()
	return c.header.GetHeaderSize()
}

func (c *DingCache) resizeHeader(blockNum, fileSize int64) error {
	c.headerLock.Lock()
	defer c.headerLock.Unlock()
	c.header.BlockNumber = blockNum
	c.header.FileSize = fileSize
	return c.header.ValidHeader()
}

// setHeaderBlock 设置头部块信息
func (c *DingCache) setHeaderBlock(blockIndex int64) error {
	c.headerLock.Lock()
	defer c.headerLock.Unlock()
	return c.header.BlockMask.Set(blockIndex)
}

// testHeaderBlock 测试头部块信息
func (c *DingCache) testHeaderBlock(blockIndex int64) (bool, error) {
	c.headerLock.Lock()
	defer c.headerLock.Unlock()
	return c.header.BlockMask.Test(blockIndex)
}

// padBlock 填充块数据
func (c *DingCache) padBlock(rawBlock []byte) []byte {
	blockSize := int64(c.getBlockSize())
	if int64(len(rawBlock)) < blockSize {
		return append(rawBlock, bytes.Repeat([]byte{0}, int(blockSize)-len(rawBlock))...)
	}
	return rawBlock
}

// Flush 刷新缓存
func (c *DingCache) Flush() error {
	if !c.isOpen {
		return errors.New("This file has been close.")
	}
	return c.flushHeader()
}

// HasBlock 检查块是否存在
func (c *DingCache) HasBlock(blockIndex int64) (bool, error) {
	return c.testHeaderBlock(blockIndex)
}

// ReadBlock 读取块数据
func (c *DingCache) ReadBlock(blockIndex int64) ([]byte, error) {
	if !c.isOpen {
		return nil, errors.New("This file has been closed.")
	}
	if int64(blockIndex) >= c.getBlockNumber() {
		return nil, errors.New("Invalid block index.")
	}
	if blockData, ok := c.blocksReadCache[blockIndex]; ok {
		return blockData, nil
	}
	hasBlock, err := c.HasBlock(blockIndex)
	if err != nil {
		return nil, err
	}
	if !hasBlock {
		return nil, nil
	}
	offset := int64(c.getHeaderSize()) + int64(blockIndex)*int64(c.getBlockSize())
	f, err := os.OpenFile(c.path, os.O_RDONLY, 0644)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := f.Seek(offset, 0); err != nil {
		return nil, err
	}
	rawBlock := make([]byte, c.getBlockSize())
	if _, err := f.Read(rawBlock); err != nil {
		return nil, err
	}
	for blockOffset := int64(1); blockOffset <= c.prefetchBlocks; blockOffset++ {
		if blockIndex+blockOffset >= c.getBlockNumber() {
			break
		}
		hasNextBlock, err := c.HasBlock(blockIndex + blockOffset)
		if err != nil {
			return nil, err
		}
		if !hasNextBlock {
			c.blocksReadCache[blockIndex+blockOffset] = nil
		} else {
			prefetchRawBlock := make([]byte, c.getBlockSize())
			if _, err := f.Read(prefetchRawBlock); err != nil {
				return nil, err
			}
			c.blocksReadCache[blockIndex+blockOffset] = c.padBlock(prefetchRawBlock)
		}
	}
	block := c.padBlock(rawBlock)
	return block, nil
}

// WriteBlock 写入块数据
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
	if err = c.setHeaderBlock(blockIndex); err != nil {
		return err
	}
	if err = c.flushHeader(); err != nil {
		return err
	}
	delete(c.blocksReadCache, blockIndex)
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
	if err := c.resizeFileSize(fileSize); err != nil {
		return err
	}
	// 设置块数量、文件大小参数
	if err := c.resizeHeader(newBlockNum, fileSize); err != nil {
		return err
	}
	return c.flushHeader()
}
