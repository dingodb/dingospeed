package util

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestCreateFileIfNotExistPreservesConcurrentBlob(t *testing.T) {
	p := filepath.Join(t.TempDir(), "blob")
	content := bytes.Repeat([]byte("cache block"), 512)
	if err := os.WriteFile(p, content, 0600); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := CreateFileIfNotExist(p); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	b, err := os.ReadFile(p)
	if err != nil || !bytes.Equal(b, content) {
		t.Fatal("existing blob was truncated", err)
	}
}
