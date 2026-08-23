package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dingospeed/internal/dao"
	"dingospeed/internal/downloader"
	"dingospeed/pkg/config"
)

func withUploadConfig(t *testing.T) {
	t.Helper()
	oldConfig := config.SysConfig
	config.SysConfig = &config.Config{
		Download: config.Download{BlockSize: 1024},
		Upload:   config.Upload{Namespace: "dingo-local", ConcurrentLimit: 4},
	}
	t.Cleanup(func() { config.SysConfig = oldConfig })
}

func validParam() dao.LocalUploadParam {
	return dao.LocalUploadParam{
		RepoType: "models",
		Org:      "dingo-local",
		Repo:     "demo",
		Revision: "main",
		FilePath: "config.json",
		Size:     10,
		Sha256:   "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	}
}

func TestValidateUploadParamAcceptsRealisticRepos(t *testing.T) {
	withUploadConfig(t)

	cases := []struct {
		name  string
		mutig func(*dao.LocalUploadParam)
	}{
		{"datasets type", func(p *dao.LocalUploadParam) { p.RepoType = "datasets" }},
		{"nested path", func(p *dao.LocalUploadParam) { p.FilePath = "subdir/nested/data.json" }},
		{"dotfile", func(p *dao.LocalUploadParam) { p.FilePath = ".gitattributes" }},
		{"leading underscore", func(p *dao.LocalUploadParam) { p.FilePath = "_internal/notes.txt" }},
		{"space in name", func(p *dao.LocalUploadParam) { p.FilePath = "docs/model card.md" }},
		{"non ascii name", func(p *dao.LocalUploadParam) { p.FilePath = "文档/说明.md" }},
		{"plus and parens", func(p *dao.LocalUploadParam) { p.FilePath = "ckpt (final)+1.bin" }},
		{"tagged revision", func(p *dao.LocalUploadParam) { p.Revision = "v1.0.2" }},
		{"zero size", func(p *dao.LocalUploadParam) { p.Size = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			param := validParam()
			tc.mutig(&param)
			if err := validateUploadParam(param); err != nil {
				t.Fatalf("expected param to be accepted: %v", err)
			}
		})
	}
}

// §9.10 路径逃逸矩阵：每个参与拼路径的字段都要覆盖多种逃逸形式。
func TestValidateUploadParamRejectionMatrix(t *testing.T) {
	withUploadConfig(t)

	badValues := []struct {
		name  string
		value string
	}{
		{"parent traversal", ".."},
		{"nested traversal", "../etc"},
		{"absolute posix", "/etc/passwd"},
		{"backslash", `sub\evil`},
		{"windows absolute", `C:\windows`},
		{"drive prefix", "C:/windows"},
		{"nul byte", "evil\x00name"},
		{"control char", "evil\nname"},
		{"dot", "."},
		{"empty", ""},
	}

	for _, bad := range badValues {
		t.Run("filePath/"+bad.name, func(t *testing.T) {
			param := validParam()
			param.FilePath = bad.value
			if err := validateUploadParam(param); err == nil {
				t.Fatalf("expected filePath %q to be rejected", bad.value)
			}
		})
		t.Run("repo/"+bad.name, func(t *testing.T) {
			param := validParam()
			param.Repo = bad.value
			if err := validateUploadParam(param); err == nil {
				t.Fatalf("expected repo %q to be rejected", bad.value)
			}
		})
		t.Run("revision/"+bad.name, func(t *testing.T) {
			param := validParam()
			param.Revision = bad.value
			if err := validateUploadParam(param); err == nil {
				t.Fatalf("expected revision %q to be rejected", bad.value)
			}
		})
		t.Run("org/"+bad.name, func(t *testing.T) {
			param := validParam()
			param.Org = bad.value
			if err := validateUploadParam(param); err == nil {
				t.Fatalf("expected org %q to be rejected", bad.value)
			}
		})
	}

	t.Run("filePath windows reserved name", func(t *testing.T) {
		param := validParam()
		param.FilePath = "nul.txt"
		if err := validateUploadParam(param); err == nil {
			t.Fatalf("expected Windows reserved name to be rejected")
		}
	})
	t.Run("filePath trailing dot", func(t *testing.T) {
		param := validParam()
		param.FilePath = "weights.bin."
		if err := validateUploadParam(param); err == nil {
			t.Fatalf("expected trailing dot to be rejected")
		}
	})
	t.Run("filePath too long", func(t *testing.T) {
		param := validParam()
		param.FilePath = strings.Repeat("a", maxSegmentLen+1)
		if err := validateUploadParam(param); err == nil {
			t.Fatalf("expected an over-long segment to be rejected")
		}
	})
	t.Run("org outside namespace", func(t *testing.T) {
		param := validParam()
		param.Org = "huggingface"
		if err := validateUploadParam(param); err == nil {
			t.Fatalf("expected org outside the reserved namespace to be rejected")
		}
	})
	t.Run("invalid repo type", func(t *testing.T) {
		param := validParam()
		param.RepoType = "spaces"
		if err := validateUploadParam(param); err == nil {
			t.Fatalf("expected an invalid repo type to be rejected")
		}
	})
	t.Run("invalid sha256", func(t *testing.T) {
		for _, sha := range []string{"", "not-a-hash", strings.ToUpper(validParam().Sha256)} {
			param := validParam()
			param.Sha256 = sha
			if err := validateUploadParam(param); err == nil {
				t.Fatalf("expected sha256 %q to be rejected", sha)
			}
		}
	})
	t.Run("size over cache format limit", func(t *testing.T) {
		param := validParam()
		param.Size = int64(downloader.DEFAULT_BLOCK_MASK_MAX)*config.SysConfig.Download.BlockSize + 1
		if err := validateUploadParam(param); err == nil {
			t.Fatalf("expected cache format capability limit to be enforced")
		}
	})
}

func TestParseDeclaredSize(t *testing.T) {
	for _, raw := range []string{"", "abc", "-1", "1.5", "9223372036854775808"} {
		if _, err := ParseDeclaredSize(raw); err == nil {
			t.Fatalf("expected size %q to be rejected", raw)
		}
	}
	if size, err := ParseDeclaredSize("42"); err != nil || size != 42 {
		t.Fatalf("expected 42, got %d (%v)", size, err)
	}
}

// §9.10 凭证：未配置 / 未提供 / 无效，三者必须可区分，且都不产生落盘副作用。
// §9.9 并发上限
func TestUploadConcurrencyLimit(t *testing.T) {
	withUploadConfig(t)
	config.SysConfig.Upload.ConcurrentLimit = 2

	svc := NewUploadService(nil)
	if !svc.acquireUploadSlot() || !svc.acquireUploadSlot() {
		t.Fatalf("expected the first two slots to be granted")
	}
	if svc.acquireUploadSlot() {
		t.Fatalf("expected the third concurrent upload to be rejected")
	}
	svc.releaseUploadSlot()
	if !svc.acquireUploadSlot() {
		t.Fatalf("expected a slot to become available after release")
	}
}

func errorCodeOf(err error) string {
	if e, ok := err.(interface{ ErrorCode() string }); ok {
		return e.ErrorCode()
	}
	return ""
}

// §9.11 防淘汰
func TestIsProtectedLocalUploadCacheFile(t *testing.T) {
	withUploadConfig(t)

	root := filepath.Join("tmp", "repos")
	protected := []string{
		filepath.Join(root, "files", "models", "dingo-local", "demo", "blobs", "abc"),
		filepath.Join(root, "files", "datasets", "dingo-local", "demo", "blobs", "abc"),
		filepath.Join(root, "files", "models", "dingo-local", "demo", "resolve", "commit", "config.json"),
	}
	for _, path := range protected {
		if !isProtectedLocalUploadCacheFile(root, path) {
			t.Fatalf("expected %s to be protected from disk clean", path)
		}
	}

	cleanable := []string{
		filepath.Join(root, "files", "models", "huggingface", "demo", "blobs", "abc"),
		filepath.Join(root, "files", "datasets", "someorg", "demo", "blobs", "abc"),
		filepath.Join(root, "files", "models", "dingo-local-lookalike", "demo", "blobs", "abc"),
	}
	for _, path := range cleanable {
		if isProtectedLocalUploadCacheFile(root, path) {
			t.Fatalf("expected %s to remain cleanable", path)
		}
	}
}

// §9.11 真实触发一次清理：公开模型按既有策略被清掉，dingo-local 一个都不能少。
func TestDiskCleanKeepsLocalUploadsAndStillCleansPublicCache(t *testing.T) {
	repos := t.TempDir()
	oldConfig := config.SysConfig
	config.SysConfig = &config.Config{
		Server: config.ServerConfig{Repos: repos, Online: true},
		Upload: config.Upload{Namespace: "dingo-local"},
		DiskClean: config.DiskClean{
			Enabled:            true,
			CacheSizeLimit:     1,
			CacheCleanStrategy: "LARGE_FIRST",
		},
	}
	defer func() { config.SysConfig = oldConfig }()

	write := func(parts ...string) string {
		path := filepath.Join(append([]string{repos}, parts...)...)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir failed: %v", err)
		}
		if err := os.WriteFile(path, make([]byte, 4096), 0o644); err != nil {
			t.Fatalf("write failed: %v", err)
		}
		// 现有清理策略会跳过当天修改过的文件，把时间往前拨才能真正触发清理。
		stale := time.Now().Add(-48 * time.Hour)
		if err := os.Chtimes(path, stale, stale); err != nil {
			t.Fatalf("chtimes failed: %v", err)
		}
		return path
	}

	localBlob := write("files", "models", "dingo-local", "demo", "blobs", "aaa")
	localResolve := write("files", "models", "dingo-local", "demo", "resolve", "commit", "config.json")
	localDataset := write("files", "datasets", "dingo-local", "demo", "blobs", "bbb")
	publicBlob := write("files", "models", "huggingface", "demo", "blobs", "ccc")

	(&SysService{}).checkDiskUsage()

	for _, path := range []string{localBlob, localResolve, localDataset} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("dingo-local content must survive disk clean, missing %s: %v", path, err)
		}
	}
	if _, err := os.Stat(publicBlob); !os.IsNotExist(err) {
		t.Fatalf("public cache must still be cleaned by the existing strategy, stat err: %v", err)
	}
}
