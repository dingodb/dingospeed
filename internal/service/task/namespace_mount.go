package task

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"

	"dingospeed/internal/dao"
	"dingospeed/pkg/common"
	"dingospeed/pkg/config"
	"dingospeed/pkg/repository"
)

// Repository API requests only target this configured service without redirects.
func repositoryRequest(ctx context.Context, k repository.RepoKey, authorization, operation string, q url.Values) (*http.Response, error) {
	if err := k.Validate(); err != nil {
		return nil, err
	}
	host := config.SysConfig.Server.Host
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	base := "http://" + net.JoinHostPort(host, strconv.Itoa(config.SysConfig.Server.Port)) + "/api/repositories/" + url.PathEscape(k.RepoType) + "/" + url.PathEscape(k.Namespace) + "/"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+operation+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	if k.Namespace == repository.ModelScope && operation == "file" {
		req.Header.Set("X-Dingo-Ensure-Cache", "1")
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	transfer := common.NewLocalTransfer(common.CacheTrace(ctx))
	req.Header.Set(common.LocalTransferHeader, transfer.ID())
	resp, err := client.Do(req)
	if err != nil {
		transfer.Wait()
	} else {
		resp.Body = &joinedLocalBody{ReadCloser: resp.Body, transfer: transfer}
	}
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusPartialContent && operation == "file" {
		var start, end, total int64
		if n, err := fmt.Sscanf(resp.Header.Get("Content-Range"), "bytes %d-%d/%d", &start, &end, &total); err == nil && n == 3 && start == 0 && total > 0 && end == total-1 {
			resp.Body = &completeRepositoryBody{ReadCloser: resp.Body, remaining: total}
			return resp, nil
		}
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("repository %s returned %d", operation, resp.StatusCode)
	}
	return resp, nil
}

// A raw provider cache may answer a complete-file range with 206. Verify the
// declared byte count before a mount publishes its temporary file.
type completeRepositoryBody struct {
	io.ReadCloser
	remaining int64
}

func (b *completeRepositoryBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.remaining -= int64(n)
	if b.remaining < 0 || (err == io.EOF && b.remaining != 0) {
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}

// ReadRepositoryMetadata shares the authenticated source dispatch used by mounts
// and preheat. The provider adapter retains its native cache representation.
func ReadRepositoryMetadata(ctx context.Context, k repository.RepoKey, revision, authorization string) (*dao.CommitHfSha, error) {
	resp, err := repositoryRequest(ctx, k, authorization, "metadata", url.Values{"repo": {k.Repo}, "revision": {revision}})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var meta dao.CommitHfSha
	if err = json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&meta); err != nil {
		return nil, err
	}
	if err = repository.Segment(meta.Sha); err != nil {
		return nil, err
	}
	for _, file := range meta.Siblings {
		if err := repository.Relative(file.Rfilename); err != nil {
			return nil, err
		}
	}
	return &meta, nil
}

// Hosted and ModelScope repositories cannot be passed as HF repo_id values.
func (m *MountCacheTask) mountViaRepositoryAPI(k repository.RepoKey, revision string) error {
	meta, err := ReadRepositoryMetadata(m.Ctx, k, revision, m.Authorization)
	if err != nil {
		return err
	}
	q := url.Values{"repo": {k.Repo}, "revision": {meta.Sha}}
	mountRoot := config.SysConfig.Cache.MountModelDir
	for _, file := range meta.Siblings {
		if err := repository.Relative(file.Rfilename); err != nil {
			return err
		}
		target := filepath.Join(mountRoot, k.RepoType, k.Namespace, filepath.FromSlash(k.Repo), filepath.FromSlash(file.Rfilename))
		if err := repository.SafePath(mountRoot, target); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		q.Set("revision", meta.Sha)
		q.Set("path", file.Rfilename)
		resp, err := repositoryRequest(m.Ctx, k, m.Authorization, "file", q)
		if err != nil {
			return err
		}
		err = writeMountedFile(target, resp.Body)
		resp.Body.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func writeMountedFile(target string, body io.Reader) error {
	f, err := os.CreateTemp(filepath.Dir(target), ".mount-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = io.Copy(f, body); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), target)
}

func (p *PreheatCacheTask) preheatViaRepositoryAPI(k repository.RepoKey) error {
	q := url.Values{"repo": {k.Repo}, "revision": {p.Sha.Sha}}
	for _, file := range p.Sha.Siblings {
		if err := repository.Relative(file.Rfilename); err != nil {
			return err
		}
		q.Set("path", file.Rfilename)
		resp, err := repositoryRequest(p.Ctx, k, p.Authorization, "file", q)
		if err != nil {
			return err
		}
		// Count bytes as the provider response is consumed. Updating only after
		// io.Copy returned made one large ModelScope file look stuck for its
		// entire download even though data was arriving normally.
		_, err = io.Copy(io.Discard, &cacheProgressReader{reader: resp.Body, add: p.stockLen.Add})
		resp.Body.Close()
		if err != nil {
			return err
		}
		if k.Namespace == repository.ModelScope && resp.Trailer.Get("X-Dingo-Cache-Complete") != "true" {
			return fmt.Errorf("ModelScope file transfer did not complete its cache: %s", file.Rfilename)
		}
	}
	return nil
}

type cacheProgressReader struct {
	reader io.Reader
	add    func(uint64) uint64
}

func (r *cacheProgressReader) Read(buffer []byte) (int, error) {
	n, err := r.reader.Read(buffer)
	if n > 0 {
		r.add(uint64(n))
	}
	return n, err
}

type joinedLocalBody struct {
	io.ReadCloser
	transfer *common.LocalTransfer
}

func (b *joinedLocalBody) Close() error { err := b.ReadCloser.Close(); b.transfer.Wait(); return err }
