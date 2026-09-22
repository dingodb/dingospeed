package cachemount

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func fixture(t *testing.T, data []byte) string {
	t.Helper()
	name := filepath.Join(t.TempDir(), "blob")
	bits := uint64(8 * ((len(data)+3)/4 + 7) / 8)
	if bits == 0 {
		bits = 8
	}
	h := make([]byte, 36+(bits+7)/8)
	copy(h, "OLAH")
	binary.LittleEndian.PutUint64(h[4:], 8)
	binary.LittleEndian.PutUint64(h[12:], 4)
	binary.LittleEndian.PutUint64(h[20:], uint64(len(data)))
	binary.LittleEndian.PutUint64(h[28:], bits)
	for i := 36; i < len(h); i++ {
		h[i] = 255
	}
	if err := os.WriteFile(name, append(h, data...), 0600); err != nil {
		t.Fatal(err)
	}
	return name
}

func TestReadRanges(t *testing.T) {
	data := []byte("0123456789abcdefg")
	name := fixture(t, data)
	before, _ := os.ReadFile(name)
	p, err := Open(name)
	if err != nil {
		t.Fatal(err)
	}
	defer p.File.Close()
	for off := 0; off <= len(data)+1; off++ {
		for length := 1; length < 24; length++ {
			b := make([]byte, length)
			n, err := p.ReadAt(b, int64(off))
			expect := []byte{}
			if off < len(data) {
				expect = data[off:min(off+length, len(data))]
			}
			if !bytes.Equal(b[:n], expect) || (n < length && err != io.EOF) {
				t.Fatalf("off=%d length=%d n=%d err=%v", off, length, n, err)
			}
		}
	}
	after, _ := os.ReadFile(name)
	if !bytes.Equal(before, after) {
		t.Fatal("source changed")
	}
}

func TestRejectMalformedAndIncomplete(t *testing.T) {
	for _, kind := range []string{"magic", "version", "zeroBlock", "hugeBitmap", "shortBitmap", "missingBlock", "truncated"} {
		t.Run(kind, func(t *testing.T) {
			name := fixture(t, []byte("abcdefgh"))
			b, _ := os.ReadFile(name)
			switch kind {
			case "magic":
				b[0] = 'X'
			case "version":
				b[4] = 9
			case "zeroBlock":
				binary.LittleEndian.PutUint64(b[12:], 0)
			case "hugeBitmap":
				binary.LittleEndian.PutUint64(b[28:], 1<<63)
			case "shortBitmap":
				b = b[:36]
			case "missingBlock":
				b[36] = 1
			case "truncated":
				b = b[:len(b)-1]
			}
			os.WriteFile(name, b, 0600)
			if p, err := Open(name); err == nil {
				p.File.Close()
				t.Fatal("accepted invalid cache")
			}
		})
	}
}

func TestPinRejectsUnsafeTree(t *testing.T) {
	name := fixture(t, []byte("abcd"))
	for _, p := range []string{"../x", "/x", "a/../x", "a\\x", ".", ""} {
		if s, err := Pin(map[string]string{p: name}); err == nil {
			s.Close()
			t.Fatalf("accepted %q", p)
		}
	}
	if s, err := Pin(map[string]string{"a": name, "a/b": name}); err == nil {
		s.Close()
		t.Fatal("accepted collision")
	}
}

func TestPinLargeSparsePayload(t *testing.T) {
	if os.Getenv("DINGO_TEST_FUSE") != "1" {
		t.Skip("opt-in large sparse file on isolated Linux filesystem")
	}
	const size = int64(512) * 1024 * 1024 * 1024
	const block = 4 * 1024 * 1024
	const bits = size / block
	name := filepath.Join(t.TempDir(), "large")
	f, err := os.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	h := make([]byte, 36+bits/8)
	copy(h, "OLAH")
	binary.LittleEndian.PutUint64(h[4:], 8)
	binary.LittleEndian.PutUint64(h[12:], block)
	binary.LittleEndian.PutUint64(h[20:], uint64(size))
	binary.LittleEndian.PutUint64(h[28:], uint64(bits))
	for i := 36; i < len(h); i++ {
		h[i] = 255
	}
	if _, err = f.Write(h); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err = f.Truncate(int64(len(h)) + size); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()
	start := time.Now()
	p, err := Open(name)
	if err != nil {
		t.Fatal(err)
	}
	defer p.File.Close()
	if p.Size != size {
		t.Fatal(p.Size)
	}
	t.Logf("512 GiB logical payload pinned in %s; header bytes=%d (sparse fixture, not a throughput test)", time.Since(start), len(h))
}
