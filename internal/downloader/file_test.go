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
	"path/filepath"
	"testing"

	"dingospeed/pkg/config"

	"go.uber.org/zap"
)

func TestFileWrite(t *testing.T) {
	var dingFile *DingCache
	var err error
	savePath := filepath.Join(t.TempDir(), "cachefile")
	fileSize := int64(8388608)
	blockSize := int64(8388608)
	if dingFile, err = NewDingCache(savePath, blockSize); err != nil {
		zap.S().Errorf("NewDingCache err.%v", err)
		return
	}
	if err = dingFile.Resize(fileSize); err != nil {
		zap.S().Errorf("Resize err.%v", err)
		return
	}
}

func TestFileWrite2(t *testing.T) {

	h := NewDingCacheHeader(1, 1, 1)
	fmt.Println(string(h.MagicNumber[:]))
	fmt.Println(string(h.MagicNumber[:]))

	fmt.Println(string(h.MagicNumber[:]))

}

func TestFileWrite3(t *testing.T) {
	size := -14
	s := make([]byte, (size+7)/8)
	fmt.Println(len(s))
}

func TestCopyPayloadExcludesHeaderAndRequiresCompleteBlocks(t *testing.T) {
	oldConfig := config.SysConfig
	config.SysConfig = &config.Config{}
	t.Cleanup(func() { config.SysConfig = oldConfig })
	cache, err := NewDingCache(filepath.Join(t.TempDir(), "cachefile"), 4)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cache.Close() }()
	if err = cache.Resize(7); err != nil {
		t.Fatal(err)
	}
	if err = cache.WriteBlock(0, []byte("abcd")); err != nil {
		t.Fatal(err)
	}
	var partial bytes.Buffer
	if _, err = cache.CopyPayload(&partial); err == nil {
		t.Fatal("incomplete cache payload was copied")
	}
	if err = cache.WriteBlock(1, []byte("efg0")); err != nil {
		t.Fatal(err)
	}
	var complete bytes.Buffer
	written, err := cache.CopyPayload(&complete)
	if err != nil || written != 7 || complete.String() != "abcdefg" {
		t.Fatalf("written=%d payload=%q err=%v", written, complete.String(), err)
	}
}
