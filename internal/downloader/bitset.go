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
