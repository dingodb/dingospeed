package dao

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestPreparedHardlinkRemainsAReference(t *testing.T) {
	root := t.TempDir()
	blob := filepath.Join(root, "blob")
	ref := filepath.Join(root, "ref")
	content := []byte("existing container bytes")
	if err := os.WriteFile(blob, content, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(blob, ref); err != nil {
		t.Skipf("hard links unsupported: %v", err)
	}
	original, err := os.Stat(ref)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := (&FileDao{}).ConstructBlobsAndFileFile(blob, ref); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	after, err := os.Stat(ref)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(original, after) {
		t.Fatal("existing hard link was recreated")
	}
	b, err := os.ReadFile(blob)
	if err != nil || !bytes.Equal(b, content) {
		t.Fatal("reference preparation modified blob", err)
	}
}
