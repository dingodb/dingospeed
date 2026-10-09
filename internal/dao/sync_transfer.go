package dao

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"dingospeed/internal/downloader"
	"dingospeed/pkg/config"
	"dingospeed/pkg/repository"
	"dingospeed/pkg/transfersettings"
	"dingospeed/pkg/util"
)

var syncBlobTransfers sync.Map

// SyncBlobAvailable reads the cache header/bitmap and disk length, never the
// payload. Official upload writers are responsible for content verification.
func (u *UploadDao) SyncBlobAvailable(k repository.RepoKey, f LocalManifestFile) (bool, error) {
	path := BlobPath(k.RepoType, k.ID(), f.Sha256)
	if err := ensureLocalUploadPathSafe(config.SysConfig.Repos(), path); err != nil {
		return false, err
	}
	key := uploadBlobLockKey(k.RepoType, k.ID(), f.Sha256)
	uploadBlobLocks.RLock(key)
	defer uploadBlobLocks.RUnlock(key)
	return syncCacheComplete(path, f.Size)
}

func syncCacheComplete(path string, size int64) (bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("cache is not a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	h := &downloader.DingCacheHeader{}
	if err = h.Read(f); err != nil {
		return false, err
	}
	if size > 0 && h.BlockNumber == 0 {
		return false, fmt.Errorf("invalid cache block count")
	}
	if size < 0 || h.FileSize != uint64(size) || info.Size() < h.GetHeaderSize()+size {
		return false, nil
	}
	for i := uint64(0); i < h.BlockNumber; i++ {
		present, err := h.BlockMask.Test(i)
		if err != nil {
			return false, err
		}
		if !present {
			return false, nil
		}
	}
	return true, nil
}

// DownloadSyncBlob uses the same block planner, workers, retry and DingCache
// writer as remote preheat. Unverified blocks stay outside the serving blob
// directory until one complete SHA256 check succeeds and atomic promotion runs.
func (u *UploadDao) DownloadSyncBlob(ctx context.Context, k repository.RepoKey, f LocalManifestFile, sourceURLs []string, onBytes func(int64)) error {
	if k.Namespace == repository.HuggingFace || k.Namespace == repository.ModelScope {
		return fmt.Errorf("uploaded sync cannot use a remote namespace")
	}
	if config.SysConfig.Download.RespChunkSize <= 0 || config.SysConfig.Download.GoroutineMaxNumPerFile <= 0 || config.SysConfig.Download.BlockSize <= 0 {
		return fmt.Errorf("invalid downloader block/chunk/concurrency configuration")
	}
	identity := sha256.Sum256([]byte(config.SysConfig.Repos() + "\x00" + k.RepoType + "\x00" + k.ID() + "\x00" + f.Sha256))
	name := hex.EncodeToString(identity[:])
	gateValue, _ := syncBlobTransfers.LoadOrStore(name, make(chan struct{}, 1))
	gate := gateValue.(chan struct{})
	select {
	case gate <- struct{}{}:
		defer func() { <-gate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if available, err := u.SyncBlobAvailable(k, f); err != nil {
		return err
	} else if available {
		return nil
	}
	if err := RegisterHosted(k.RepoType, k.Namespace, k.Repo); err != nil {
		return err
	}
	stage := filepath.Join(config.SysConfig.Repos(), ".repository-sync", "content", name+".partial")
	if err := ensureLocalUploadPathSafe(config.SysConfig.Repos(), stage); err != nil {
		return err
	}
	if err := util.MakeDirs(stage); err != nil {
		return err
	}
	if existing, err := os.Open(stage); err == nil {
		header := &downloader.DingCacheHeader{}
		readErr := header.Read(existing)
		existing.Close()
		if readErr != nil || header.FileSize != uint64(f.Size) {
			if err := os.Remove(stage); err != nil {
				return err
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	var last error
	// Empty files need no source, but still pass the same one-time hash gate.
	if f.Size == 0 {
		d, err := downloader.NewDingCache(stage, config.SysConfig.Download.BlockSize)
		if err != nil {
			return err
		}
		if err = d.Close(); err != nil {
			return err
		}
	} else {
		for _, endpoint := range sourceURLs {
			if err := ctx.Err(); err != nil {
				return err
			}
			last = u.downloadSyncSource(ctx, k, f, stage, endpoint, onBytes)
			if last == nil {
				break
			}
		}
		if last != nil {
			return last
		}
		if len(sourceURLs) == 0 {
			return fmt.Errorf("no available node copy; uploaded repositories cannot fetch an origin")
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	complete, err := syncCacheComplete(stage, f.Size)
	if err != nil {
		return err
	}
	if !complete {
		return fmt.Errorf("downloaded cache is incomplete")
	}
	// One sequential disk pass after all range writers have exited. Do not run
	// it on reuse, source reads, publication, or task completion.
	input, err := os.OpenFile(stage, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	h := &downloader.DingCacheHeader{}
	err = h.Read(input)
	digest := sha256.New()
	if err == nil {
		_, err = io.CopyN(digest, syncContextReader{ctx, input}, f.Size)
	}
	if err == nil {
		err = input.Sync()
	}
	input.Close()
	if err != nil {
		return err
	}
	if hex.EncodeToString(digest.Sum(nil)) != f.Sha256 {
		if err = os.Remove(stage); err != nil {
			return fmt.Errorf("sha256 mismatch; cannot discard unverified staging: %w", err)
		}
		return fmt.Errorf("downloaded sha256 does not match fixed manifest")
	}
	// Share the upload blob lock only for the final check/rename, never network.
	key := uploadBlobLockKey(k.RepoType, k.ID(), f.Sha256)
	uploadBlobLocks.Lock(key)
	defer uploadBlobLocks.Unlock(key)
	final := BlobPath(k.RepoType, k.ID(), f.Sha256)
	if err = ensureLocalUploadPathSafe(config.SysConfig.Repos(), final); err != nil {
		return err
	}
	if good, err := syncCacheComplete(final, f.Size); err != nil {
		return err
	} else if good {
		return nil
	}
	if err = util.MakeDirs(final); err != nil {
		return err
	}
	return os.Rename(stage, final)
}

func (u *UploadDao) downloadSyncSource(ctx context.Context, k repository.RepoKey, f LocalManifestFile, stage, endpoint string, onBytes func(int64)) error {
	base, err := url.Parse(endpoint)
	if err != nil || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") || base.User != nil || base.Fragment != "" {
		return fmt.Errorf("invalid registered content URL")
	}
	ctx, cancel := context.WithCancel(transfersettings.WithBackground(ctx))
	defer cancel()
	client := &http.Client{Timeout: 30 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	source := &downloader.RemoteSource{Domain: base.Scheme + "://" + base.Host}
	source.Fetch = func(ctx context.Context, _ string, _ string, headers map[string]string, consume func(*http.Response) error) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return err
		}
		for key, value := range headers {
			req.Header.Set(key, value)
		}
		start, end, err := parseSyncRange(req.Header.Get("Range"), f.Size)
		if err != nil {
			return err
		}
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end-1))
		req.Header.Set("Accept-Encoding", "identity")
		resp, err := transfersettings.Do("peer", client, req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		want := fmt.Sprintf("bytes %d-%d/%d", start, end-1, f.Size)
		if resp.StatusCode != http.StatusPartialContent || resp.Header.Get("Content-Range") != want || resp.ContentLength != end-start || resp.Header.Get("Content-Encoding") != "" {
			return fmt.Errorf("node returned invalid ranged content: HTTP %d", resp.StatusCode)
		}
		resp.Body = &syncCountingBody{ReadCloser: resp.Body, onBytes: onBytes}
		return consume(resp)
	}
	p := &downloader.TaskParam{NodeSourceOnly: true, RepoKey: k, LocalOnly: true, Source: source, Context: ctx, Cancel: cancel, BlobsFile: stage, FileName: f.Path, FileSize: f.Size, DataType: k.RepoType, OrgRepo: k.ID(), Etag: f.Sha256, Unordered: true, ResponseChan: make(chan []byte, 4), CacheResult: make(chan error, 1)}
	if err = NewDownloaderDao(nil).FileDownload(0, f.Size, true, p); err != nil {
		return err
	}
	for range p.ResponseChan {
	} // Join producers and cache writers before return.
	return <-p.CacheResult
}

// ParseSyncRange accepts only one explicit byte range; no suffix/multipart or
// ambiguous bounds are permitted on this node-internal transfer endpoint.
func ParseSyncRange(raw string, size int64) (int64, int64, error) { return parseSyncRange(raw, size) }
func parseSyncRange(raw string, size int64) (int64, int64, error) {
	if raw == "" {
		return 0, size, nil
	}
	if !strings.HasPrefix(raw, "bytes=") {
		return 0, 0, fmt.Errorf("invalid byte range")
	}
	parts := strings.Split(strings.TrimPrefix(raw, "bytes="), "-")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("one explicit byte range is required")
	}
	for _, part := range parts {
		for _, digit := range part {
			if digit < '0' || digit > '9' {
				return 0, 0, fmt.Errorf("invalid range digit")
			}
		}
	}
	a, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, 0, err
	}
	b, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || a < 0 || b < a || b >= size {
		return 0, 0, fmt.Errorf("byte range is outside file")
	}
	return a, b + 1, nil
}

type syncContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r syncContextReader) Read(b []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(b)
}

type syncCountingBody struct {
	io.ReadCloser
	onBytes func(int64)
}

func (b *syncCountingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 && b.onBytes != nil {
		b.onBytes(int64(n))
	}
	return n, err
}
