package downloader

import (
	"path/filepath"
	"sync"
	"testing"
)

// TestConcurrentResizeAndHeaderReads 盯住 Resize 与 header 读者之间的互斥。
//
// resizeHeader 会改写 header 的 BlockNumber 与 FileSize，而 GetFileSize /
// getBlockNumber / HasBlock 都在读同一份 header。这两侧必须真正互斥——
// 它曾经不是：resizeHeader 取的是 headerLock.RLock（读锁），读锁之间互不排斥，
// 于是「一边改 header 一边读 header」是一个数据竞争。
func TestConcurrentResizeAndHeaderReads(t *testing.T) {
	const blockSize = 16
	const steps = 64

	path := filepath.Join(t.TempDir(), "resize")
	c, err := NewDingCache(path, blockSize)
	if err != nil {
		t.Fatalf("NewDingCache failed: %v", err)
	}
	defer c.Close()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// Resize 只允许增大，逐级放大即可。
		for i := int64(1); i <= steps; i++ {
			if err := c.Resize(i * blockSize); err != nil {
				t.Errorf("Resize(%d) failed: %v", i*blockSize, err)
				return
			}
		}
	}()
	for reader := 0; reader < 4; reader++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				_ = c.GetFileSize()
				_ = c.getBlockNumber()
				if _, err := c.HasBlock(0); err != nil {
					t.Errorf("HasBlock(0) failed: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	if got := c.GetFileSize(); got != steps*blockSize {
		t.Fatalf("final file size is %d, want %d", got, steps*blockSize)
	}
	if got := c.getBlockNumber(); got != steps {
		t.Fatalf("final block number is %d, want %d", got, steps)
	}
}
