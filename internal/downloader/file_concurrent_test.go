package downloader

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestConcurrentHasBlockAndWriteBlock 复现下载侧的既有并发形态：
// 多个 range 任务共享同一个 DingCache，各自「查位 → 未置位则写块」
// （remote_task.go:124-131、:169-177），分块上传沿用同一套。
//
// 这个测试的价值全在 -race 下：位图的读路径（HasBlock → headerLock）与写路径
// （WriteBlock → setHeaderBlock → fileLock）曾经走两把互不排斥的锁，
// 于是「一边查位一边置位」是一个真正的数据竞争。
func TestConcurrentHasBlockAndWriteBlock(t *testing.T) {
	const blockSize = 16
	const blockNum = 64

	path := filepath.Join(t.TempDir(), "concurrent")
	c, err := NewDingCache(path, blockSize)
	if err != nil {
		t.Fatalf("NewDingCache failed: %v", err)
	}
	defer c.Close()
	if err = c.Resize(blockSize * blockNum); err != nil {
		t.Fatalf("Resize failed: %v", err)
	}

	// 每个块由两个 goroutine 同时争抢，放大「查位」与「置位」的交错。
	var wg sync.WaitGroup
	for i := int64(0); i < blockNum; i++ {
		for dup := 0; dup < 2; dup++ {
			wg.Add(1)
			go func(i int64) {
				defer wg.Done()
				block := make([]byte, blockSize)
				for j := range block {
					block[j] = byte(i)
				}
				has, err := c.HasBlock(i)
				if err != nil {
					t.Errorf("HasBlock(%d) failed: %v", i, err)
					return
				}
				if !has {
					if err = c.WriteBlock(i, block); err != nil {
						t.Errorf("WriteBlock(%d) failed: %v", i, err)
					}
				}
			}(i)
		}
	}
	wg.Wait()

	// 收尾状态必须是确定的：每个块都被置位，且内容正确。
	for i := int64(0); i < blockNum; i++ {
		has, err := c.HasBlock(i)
		if err != nil {
			t.Fatalf("HasBlock(%d) failed: %v", i, err)
		}
		if !has {
			t.Fatalf("block %d was never marked as written", i)
		}
	}

	// 直接读原始文件，绕开 ReadBlock 的块缓存（它依赖全局配置）。
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open cache file failed: %v", err)
	}
	defer f.Close()
	payload := make([]byte, blockSize*blockNum)
	if _, err = f.ReadAt(payload, c.getHeaderSize()); err != nil {
		t.Fatalf("read payload failed: %v", err)
	}
	for i := int64(0); i < blockNum; i++ {
		for j := int64(0); j < blockSize; j++ {
			if got := payload[i*blockSize+j]; got != byte(i) {
				t.Fatalf("block %d byte %d is %d, want %d", i, j, got, byte(i))
			}
		}
	}
}
