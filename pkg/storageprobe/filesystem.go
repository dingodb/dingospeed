package storageprobe

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
)

const Directory = ".dingospeed-probe"
const Sentinel = "dingospeed-storage-probe-v1\n"

var errSentinel = errors.New("invalid probe sentinel or directory")

// Filesystem performs no I/O during construction. The dedicated directory and
// sentinel must be provisioned beforehand on the intended filesystem.
func Filesystem(repos string) (Operation, Operation) {
	dir := filepath.Join(repos, Directory)
	read := func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := os.Lstat(dir)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errSentinel
		}
		path := filepath.Join(dir, "sentinel")
		info, err = os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errSentinel
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		data, readErr := io.ReadAll(io.LimitReader(f, int64(len(Sentinel)+1)))
		closeErr := f.Close()
		if err := errors.Join(readErr, closeErr); err != nil {
			return err
		}
		if string(data) != Sentinel {
			return errSentinel
		}
		return ctx.Err()
	}
	// Only one write worker uses this closure. A failed cleanup blocks new file
	// creation on subsequent attempts, bounding leftover files to one per process.
	var pending string
	write := func(ctx context.Context) (err error) {
		if err = read(ctx); err != nil {
			return err
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		if pending != "" {
			if err = os.Remove(pending); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			pending = ""
		}
		f, err := os.CreateTemp(dir, "write-*")
		if err != nil {
			return err
		}
		pending = f.Name()
		defer func() {
			closeErr := f.Close()
			removeErr := os.Remove(pending)
			if removeErr == nil || errors.Is(removeErr, os.ErrNotExist) {
				pending = ""
				removeErr = nil
			}
			err = errors.Join(err, closeErr, removeErr)
		}()
		if err = ctx.Err(); err != nil {
			return err
		}
		if _, err = f.WriteString(Sentinel); err != nil {
			return err
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		return f.Sync()
	}
	return read, write
}
