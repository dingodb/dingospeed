package cachemount

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
)

// Opt in because CI hosts may lack /dev/fuse. An opted-in failure is fatal,
// never skipped, so a successful report proves an actual kernel mount.
func TestRealMount(t *testing.T) {
	if os.Getenv("DINGO_TEST_FUSE") != "1" {
		t.Skip("set DINGO_TEST_FUSE=1 for real kernel mount")
	}
	data := bytes.Repeat([]byte("0123456789abcdef"), 4096)
	name := fixture(t, data)
	s, err := Pin(map[string]string{"sub/weights.bin": name})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	mount := t.TempDir()
	server, err := Mount(s, mount)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := server.Unmount(); err != nil {
			t.Error(err)
		}
		server.Wait()
	}()
	p := filepath.Join(mount, "sub", "weights.bin")
	st, err := os.Stat(p)
	if err != nil || st.Size() != int64(len(data)) {
		t.Fatalf("stat %v %v", st, err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				b, err := os.ReadFile(p)
				if err != nil || !bytes.Equal(b, data) {
					t.Errorf("concurrent read: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b, err := syscall.Mmap(int(f.Fd()), 0, len(data), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, data) {
		t.Error("mmap differs")
	}
	syscall.Munmap(b)
	if err := os.WriteFile(p, []byte("oops"), 0600); err == nil {
		t.Fatal("write accepted")
	}
	if err := os.Remove(p); err == nil {
		t.Fatal("unlink accepted")
	}
	if err := os.Mkdir(filepath.Join(mount, "new"), 0700); err == nil {
		t.Fatal("mkdir accepted")
	}
	// Unlink and replace the pathname; pinned descriptor must retain old data.
	if err := os.Remove(name); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(p)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("pin lost after unlink: %v", err)
	}
}

func TestTruncationReturnsIOError(t *testing.T) {
	name := fixture(t, []byte("abcdefgh"))
	p, err := Open(name)
	if err != nil {
		t.Fatal(err)
	}
	defer p.File.Close()
	if err := os.Truncate(name, p.Offset+2); err != nil {
		t.Fatal(err)
	}
	n := fileNode{payload: p}
	if _, errno := n.Read(context.Background(), nil, make([]byte, 8), 0); errno != syscall.EIO {
		t.Fatalf("got %v", errno)
	}
}
