package downloader

import (
	"errors"
	"fmt"
)

// Bitset 结构体表示一个位集合
type Bitset struct {
	size int64
	bits []byte
}

// NewBitset 创建一个新的 Bitset 对象
func NewBitset(size int64) *Bitset {
	return &Bitset{
		size: size,
		bits: make([]byte, (size+7)/8),
	}
}

// Set 将指定索引的位设置为 1
func (b *Bitset) Set(index int64) error {
	if index < 0 || index >= b.size {
		return errors.New("Index out of range")
	}
	byteIndex := index / 8
	bitIndex := index % 8
	b.bits[byteIndex] |= 1 << bitIndex
	return nil
}

// Clear 将指定索引的位设置为 0
func (b *Bitset) Clear(index int64) error {
	if index < 0 || index >= b.size {
		return errors.New("Index out of range")
	}
	byteIndex := index / 8
	bitIndex := index % 8
	b.bits[byteIndex] &= ^(1 << bitIndex)
	return nil
}

// Test 检查指定索引的位的值
func (b *Bitset) Test(index int64) (bool, error) {
	if index < 0 || index >= b.size {
		return false, errors.New("Index out of range")
	}
	byteIndex := index / 8
	bitIndex := index % 8
	return (b.bits[byteIndex] & (1 << bitIndex)) != 0, nil
}

// String 返回 Bitset 的字符串表示
func (b *Bitset) String() string {
	result := ""
	for _, byteVal := range b.bits {
		result += fmt.Sprintf("%08b", byteVal)
	}
	return result
}
