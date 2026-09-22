// Package cachemount exposes complete DingCache payloads without copying or
// modifying cache files. All backing descriptors remain open until unmount.
package cachemount

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"strings"
)

type Payload struct {
	File   *os.File
	Offset int64
	Size   int64
}

// Open accepts OLAH v8 only. A damaged container never falls back to raw data.
func Open(name string) (*Payload, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	p, err := inspect(f)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return p, nil
}

func inspect(f *os.File) (*Payload, error) {
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular cache file")
	}
	var h [36]byte
	if _, err := f.ReadAt(h[:], 0); err != nil {
		return nil, err
	}
	v := binary.LittleEndian.Uint64(h[4:12])
	block := binary.LittleEndian.Uint64(h[12:20])
	size := binary.LittleEndian.Uint64(h[20:28])
	bits := binary.LittleEndian.Uint64(h[28:36])
	if string(h[:4]) != "OLAH" || v != 8 || block == 0 || block > math.MaxInt64 || size > math.MaxInt64 || bits == 0 || bits > 8*1024*1024 {
		return nil, fmt.Errorf("invalid OLAH v8 header")
	}
	count := size / block
	if size%block != 0 {
		count++
	}
	if count > bits {
		return nil, fmt.Errorf("bitmap too small")
	}
	mask := make([]byte, (bits+7)/8)
	if _, err := f.ReadAt(mask, 36); err != nil {
		return nil, err
	}
	offset := int64(36 + len(mask))
	if st.Size()-offset < int64(size) {
		return nil, fmt.Errorf("truncated cache payload")
	}
	for i := uint64(0); i < count; i++ {
		if mask[i/8]&(1<<(i%8)) == 0 {
			return nil, fmt.Errorf("missing cache block %d", i)
		}
	}
	return &Payload{File: f, Offset: offset, Size: int64(size)}, nil
}

func (p *Payload) ReadAt(b []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("negative offset")
	}
	if len(b) == 0 {
		return 0, nil
	}
	if off >= p.Size {
		return 0, io.EOF
	}
	want := len(b)
	if int64(len(b)) > p.Size-off {
		b = b[:p.Size-off]
	}
	n, err := p.File.ReadAt(b, p.Offset+off)
	if err == nil && n < want {
		err = io.EOF
	}
	return n, err
}

type Snapshot struct {
	Files       map[string]*Payload
	Directories map[string]string
}

// Pin validates the whole tree before exposing any file. It reads headers and
// bitmaps only, not the weights. Failure closes all previously opened files.
func Pin(sources map[string]string) (*Snapshot, error) {
	s := &Snapshot{Files: make(map[string]*Payload)}
	for name, source := range sources {
		if name == "." || !validPath(name) {
			s.Close()
			return nil, fmt.Errorf("invalid model path %q", name)
		}
		p, err := Open(source)
		if err != nil {
			s.Close()
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		s.Files[name] = p
	}
	for name := range s.Files {
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if _, ok := s.Files[parent]; ok {
				s.Close()
				return nil, fmt.Errorf("file/directory collision: %s", parent)
			}
		}
	}
	if len(s.Files) == 0 {
		return nil, fmt.Errorf("empty model manifest")
	}
	return s, nil
}
func validPath(p string) bool {
	return p != "" && !strings.ContainsAny(p, "\\\x00") && !strings.HasPrefix(p, "/") && path.Clean(p) == p && p != ".." && !strings.HasPrefix(p, "../")
}
func (s *Snapshot) Close() {
	for _, p := range s.Files {
		p.File.Close()
	}
}
