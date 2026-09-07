package util

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestPathExistsResults(t *testing.T) {
	dir := t.TempDir()
	exists, err := PathExists(filepath.Join(dir, "missing"))
	if err != nil || exists {
		t.Fatalf("missing path: exists=%v err=%v", exists, err)
	}

	path := filepath.Join(dir, "present")
	if err := os.WriteFile(path, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	exists, err = PathExists(path)
	if err != nil || !exists {
		t.Fatalf("existing path: exists=%v err=%v", exists, err)
	}
}

func TestClassifyWrappedFileAccessError(t *testing.T) {
	original := &os.PathError{Op: "read", Path: "/repos/meta.json", Err: syscall.Errno(107)}
	accessErr, ok := ClassifyFileAccessError("/repos/meta.json", fmt.Errorf("cache read: %w", original))
	if !ok {
		t.Fatal("expected wrapped filesystem error to be classified")
	}
	if accessErr.Kind != FileAccessMountDisconnected {
		t.Fatalf("got kind %q, want %q", accessErr.Kind, FileAccessMountDisconnected)
	}
	if _, ok = ClassifyFileAccessError("/repos/meta.json", errors.New("invalid metadata")); ok {
		t.Fatal("content error must not be classified as a storage access failure")
	}
}

func TestClassifyFileAccessError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want FileAccessErrorKind
	}{
		{name: "permission", err: os.ErrPermission, want: FileAccessPermissionDenied},
		{name: "disconnected", err: &os.PathError{Op: "stat", Path: "/repos", Err: syscall.Errno(107)}, want: FileAccessMountDisconnected},
		{name: "stale", err: &os.PathError{Op: "stat", Path: "/repos", Err: syscall.Errno(116)}, want: FileAccessMountDisconnected},
		{name: "io", err: &os.PathError{Op: "stat", Path: "/repos", Err: syscall.Errno(5)}, want: FileAccessIOFailure},
		{name: "other", err: errors.New("other"), want: FileAccessUnavailable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyFileAccessErrorForOS(tc.err, "linux"); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
