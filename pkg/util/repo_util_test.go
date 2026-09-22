package util

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
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
	original := &os.PathError{Op: "read", Path: "/repos/meta.json", Err: os.ErrPermission}
	accessErr, ok := ClassifyFileAccessError("/repos/meta.json", fmt.Errorf("cache read: %w", original))
	if !ok {
		t.Fatal("expected wrapped filesystem error to be classified")
	}
	if accessErr.Kind != FileAccessPermissionDenied || !errors.Is(accessErr, original) {
		t.Fatalf("classification or original cause lost: %v", accessErr)
	}
	if _, ok = ClassifyFileAccessError("/repos/meta.json", errors.New("invalid metadata")); ok {
		t.Fatal("content error must not be classified as a storage access failure")
	}
	link := &os.LinkError{Op: "rename", Old: "tmp", New: "meta.json", Err: os.ErrPermission}
	classified, ok := ClassifyFileAccessError("meta.json", fmt.Errorf("atomic write: %w", link))
	if !ok || classified.Kind != FileAccessPermissionDenied || !errors.Is(classified, link) {
		t.Fatal("rename failure or original error lost")
	}
}

func TestFileAccessPlatformSemantics(t *testing.T) {
	for _, tc := range []struct {
		platform string
		code     syscall.Errno
		want     FileAccessErrorKind
	}{
		{"linux", 5, FileAccessIOFailure}, {"linux", 13, FileAccessPermissionDenied},
		{"linux", 1, FileAccessPermissionDenied}, {"linux", 107, FileAccessMountDisconnected},
		{"linux", 116, FileAccessMountDisconnected}, {"linux", 999, FileAccessUnavailable},
		{"windows", 5, FileAccessPermissionDenied}, {"windows", 107, FileAccessUnavailable},
		{"windows", 116, FileAccessUnavailable}, {"darwin", 107, FileAccessUnavailable},
	} {
		t.Run(fmt.Sprintf("%s/%d", tc.platform, tc.code), func(t *testing.T) {
			err := fmt.Errorf("wrapped: %w", &os.PathError{Op: "read", Path: "p", Err: tc.code})
			if got := classifyFileAccessErrorForOS(err, tc.platform); got != tc.want {
				t.Fatalf("got %s want %s", got, tc.want)
			}
			if tc.platform == runtime.GOOS && classifyFileAccessError(err) != tc.want {
				t.Fatal("native classification differs")
			}
		})
	}
}

func TestFileAccessObservationIsBoundedAndIgnoresNonAccessErrors(t *testing.T) {
	before := FileAccessObservations()
	for _, err := range []error{nil, errors.New("invalid JSON"), fmt.Errorf("wrapped: %w", &os.PathError{Op: "read", Path: "p", Err: os.ErrNotExist})} {
		if _, ok := ClassifyFileAccessError("p", err); ok {
			t.Fatalf("unexpected classification: %v", err)
		}
		ObserveFileAccessFailure("read", err)
	}
	err := fmt.Errorf("wrapped: %w", &os.PathError{Op: "read", Path: "p", Err: os.ErrPermission})
	ObserveFileAccessFailure("unbounded-operation", err)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); ObserveFileAccessFailure("read", err) }()
	}
	wg.Wait()
	after := FileAccessObservations()
	if len(after) != 16 {
		t.Fatalf("unbounded observations: %d", len(after))
	}
	for i, observation := range after {
		want := before[i].Count
		if observation.Operation == "read" && observation.Kind == FileAccessPermissionDenied {
			want += 32
		}
		if observation.Count != want {
			t.Fatalf("%+v want count %d", observation, want)
		}
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
