package dao

import (
	"fmt"
	"testing"

	"dingospeed/pkg/config"
)

// 上传侧与下载侧必须为同一个文件算出逐字符相同的路径：DingCacheManager 以路径为 key
// 保证进程内唯一句柄，差一个字符就是两份互相看不见的块位图。
func TestBlobPathIsIdenticalForUploadAndDownload(t *testing.T) {
	const etag = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

	cases := []struct {
		name  string
		repos string
	}{
		{"plain", "/var/lib/dingospeed/repos"},
		// 配置里带结尾斜杠是很自然的写法，路径拼接必须吸收掉它。
		{"trailing slash", "/var/lib/dingospeed/repos/"},
		{"redundant separators", "/var/lib//dingospeed/repos"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			oldConfig := config.SysConfig
			config.SysConfig = &config.Config{Server: config.ServerConfig{Repos: tc.repos}}
			t.Cleanup(func() { config.SysConfig = oldConfig })

			download := BlobPath("models", "dingo-local/demo", etag)
			upload := localBlobPath("models", "dingo-local/demo", etag)
			if download != upload {
				t.Fatalf("upload and download disagree on the blob path:\n  download=%q\n  upload  =%q", download, upload)
			}

			// 同一个 repos 的两种写法（带不带结尾斜杠）也必须收敛到同一个字符串，
			// 否则同一台机器换个配置写法就会产生两份位图。
			config.SysConfig = &config.Config{Server: config.ServerConfig{Repos: "/var/lib/dingospeed/repos"}}
			if canonical := BlobPath("models", "dingo-local/demo", etag); canonical != download {
				t.Fatalf("blob path is not canonical: %q vs %q", canonical, download)
			}
		})
	}
}

// 记录被替换掉的旧写法为什么不行：结尾斜杠会多出一个分隔符，
// 于是同一个文件在 DingCacheManager 里变成两个 key。
func TestLegacySprintfBlobPathWasNotCanonical(t *testing.T) {
	const etag = "abc"
	legacy := func(repos string) string {
		blobsDir := fmt.Sprintf("%s/files/%s/%s/blobs", repos, "models", "dingo-local/demo")
		return fmt.Sprintf("%s/%s", blobsDir, etag)
	}
	if legacy("/repos") == legacy("/repos/") {
		t.Fatalf("test premise is stale: the legacy form apparently canonicalizes now")
	}

	oldConfig := config.SysConfig
	t.Cleanup(func() { config.SysConfig = oldConfig })
	config.SysConfig = &config.Config{Server: config.ServerConfig{Repos: "/repos"}}
	withoutSlash := BlobPath("models", "dingo-local/demo", etag)
	config.SysConfig = &config.Config{Server: config.ServerConfig{Repos: "/repos/"}}
	withSlash := BlobPath("models", "dingo-local/demo", etag)
	if withoutSlash != withSlash {
		t.Fatalf("BlobPath must canonicalize the trailing slash: %q vs %q", withoutSlash, withSlash)
	}
}
