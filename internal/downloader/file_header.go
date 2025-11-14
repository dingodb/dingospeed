package downloader

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
)

var magicNumber = [4]byte{'O', 'L', 'A', 'H'}

// DingCacheHeader 结构体表示 Olah 缓存文件的头部
type DingCacheHeader struct {
	MagicNumber   [4]byte
	Version       uint64
	BlockSize     uint64
	FileSize      uint64
	BlockMaskSize uint64
	BlockNumber   uint64
	BlockMask     *Bitset
}

func NewDingCacheHeader(version, blockSize, fileSize uint64) *DingCacheHeader {
	blockNumber := (fileSize + blockSize - 1) / blockSize
	blockMask := NewBitset(DEFAULT_BLOCK_MASK_MAX)
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
	if h.BlockSize == 0 {
		return errors.New("BlockSize cannot be 0")
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
