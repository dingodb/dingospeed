package cachemount

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type fileNode struct {
	fs.Inode
	payload *Payload
}

func (n *fileNode) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = syscall.S_IFREG | 0444
	out.Size = uint64(n.payload.Size)
	return 0
}
func (n *fileNode) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	if flags&uint32(syscall.O_ACCMODE|syscall.O_TRUNC|syscall.O_APPEND) != 0 {
		return nil, 0, syscall.EROFS
	}
	return nil, fuse.FOPEN_KEEP_CACHE, 0
}
func (n *fileNode) Read(ctx context.Context, f fs.FileHandle, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	count, err := n.payload.ReadAt(dest, off)
	// EOF within the declared range is truncation, never a successful short read.
	want := int64(len(dest))
	if off >= n.payload.Size {
		want = 0
	} else if want > n.payload.Size-off {
		want = n.payload.Size - off
	}
	if off < 0 || (err != nil && err != io.EOF) || int64(count) != want {
		return nil, syscall.EIO
	}
	return fuse.ReadResultData(dest[:count]), 0
}

type rootNode struct {
	fs.Inode
	snapshot *Snapshot
}

type unavailableDir struct{ fs.Inode }

func (n *unavailableDir) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	return nil, syscall.EIO
}
func (n *unavailableDir) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	return nil, syscall.EIO
}

func ensureDir(ctx context.Context, root *fs.Inode, parts []string) *fs.Inode {
	dir := root
	for _, part := range parts {
		next := dir.GetChild(part)
		if next == nil {
			next = dir.NewPersistentInode(ctx, &fs.Inode{}, fs.StableAttr{Mode: syscall.S_IFDIR})
			dir.AddChild(part, next, false)
		}
		dir = next
	}
	return dir
}

func (r *rootNode) OnAdd(ctx context.Context) {
	for name, reason := range r.snapshot.Directories {
		parts := strings.Split(name, "/")
		parent := ensureDir(ctx, &r.Inode, parts[:len(parts)-1])
		if reason != "" {
			parent.AddChild(parts[len(parts)-1], parent.NewPersistentInode(ctx, &unavailableDir{}, fs.StableAttr{Mode: syscall.S_IFDIR}), false)
		} else {
			ensureDir(ctx, parent, parts[len(parts)-1:])
		}
	}
	for name, p := range r.snapshot.Files {
		parts := strings.Split(name, "/")
		dir := &r.Inode
		for _, part := range parts[:len(parts)-1] {
			next := dir.GetChild(part)
			if next == nil {
				next = dir.NewPersistentInode(ctx, &fs.Inode{}, fs.StableAttr{Mode: syscall.S_IFDIR})
				dir.AddChild(part, next, false)
			}
			dir = next
		}
		n := dir.NewPersistentInode(ctx, &fileNode{payload: p}, fs.StableAttr{Mode: syscall.S_IFREG})
		dir.AddChild(parts[len(parts)-1], n, false)
	}
}

// Mount requires an existing empty directory, never hides existing user files.
// The caller owns Snapshot until the returned server has finished serving.
func Mount(s *Snapshot, mountpoint string) (*fuse.Server, error) {
	entries, err := os.ReadDir(mountpoint)
	if err != nil {
		return nil, err
	}
	if len(entries) != 0 {
		return nil, fmt.Errorf("mountpoint must be empty")
	}
	return fs.Mount(mountpoint, &rootNode{snapshot: s}, &fs.Options{
		MountOptions: fuse.MountOptions{Options: []string{"ro", "default_permissions"}, FsName: "dingocache", Name: "dingocache"},
	})
}
