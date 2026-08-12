package service

import (
	"fmt"
	"strings"
	"testing"

	"dingospeed/internal/dao"
	"dingospeed/internal/downloader"
	"dingospeed/pkg/config"
)

const testSha = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func validPublishParam() dao.LocalPublishParam {
	return dao.LocalPublishParam{
		RepoType: "models",
		Org:      "dingo-local",
		Repo:     "demo",
		Revision: "main",
		Files: []dao.LocalManifestFile{
			{Path: "config.json", Sha256: testSha, Size: 10},
		},
	}
}

func TestValidatePublishParamAcceptsRealisticBatches(t *testing.T) {
	withUploadConfig(t, "secret")

	cases := []struct {
		name  string
		mutig func(*dao.LocalPublishParam)
	}{
		{"datasets type", func(p *dao.LocalPublishParam) { p.RepoType = "datasets" }},
		{"tagged revision", func(p *dao.LocalPublishParam) { p.Revision = "v1.0.2" }},
		{"nested and unusual names", func(p *dao.LocalPublishParam) {
			p.Files = []dao.LocalManifestFile{
				{Path: "subdir/nested/data.json", Sha256: testSha, Size: 1},
				{Path: ".gitattributes", Sha256: testSha, Size: 2},
				{Path: "docs/model card.md", Sha256: testSha, Size: 3},
				{Path: "文档/说明.md", Sha256: testSha, Size: 4},
				{Path: "ckpt (final)+1.bin", Sha256: testSha, Size: 0},
			}
		}},
		{"at the file count limit", func(p *dao.LocalPublishParam) {
			max := config.SysConfig.GetUploadPublishMaxFiles()
			p.Files = make([]dao.LocalManifestFile, 0, max)
			for i := 0; i < max; i++ {
				p.Files = append(p.Files, dao.LocalManifestFile{
					Path: fmt.Sprintf("shard-%04d.bin", i), Sha256: testSha, Size: 1,
				})
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			param := validPublishParam()
			tc.mutig(&param)
			if err := validatePublishParam(param); err != nil {
				t.Fatalf("expected publish param to be accepted: %v", err)
			}
		})
	}
}

// §9.10：发布清单是一个新的、批量的不可信输入入口，路径逃逸矩阵必须在这里重跑一遍，
// 不能因为“上传时已经校验过”就跳过。
func TestValidatePublishParamRejectionMatrix(t *testing.T) {
	withUploadConfig(t, "secret")

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
		t.Run("path/"+bad.name, func(t *testing.T) {
			param := validPublishParam()
			param.Files[0].Path = bad.value
			if err := validatePublishParam(param); err == nil {
				t.Fatalf("expected path %q to be rejected", bad.value)
			}
		})
		t.Run("path in second entry/"+bad.name, func(t *testing.T) {
			param := validPublishParam()
			param.Files = append(param.Files, dao.LocalManifestFile{Path: bad.value, Sha256: testSha, Size: 1})
			if err := validatePublishParam(param); err == nil {
				t.Fatalf("expected path %q to be rejected even when it is not the first entry", bad.value)
			}
		})
		t.Run("repo/"+bad.name, func(t *testing.T) {
			param := validPublishParam()
			param.Repo = bad.value
			if err := validatePublishParam(param); err == nil {
				t.Fatalf("expected repo %q to be rejected", bad.value)
			}
		})
		t.Run("revision/"+bad.name, func(t *testing.T) {
			param := validPublishParam()
			param.Revision = bad.value
			if err := validatePublishParam(param); err == nil {
				t.Fatalf("expected revision %q to be rejected", bad.value)
			}
		})
		t.Run("org/"+bad.name, func(t *testing.T) {
			param := validPublishParam()
			param.Org = bad.value
			if err := validatePublishParam(param); err == nil {
				t.Fatalf("expected org %q to be rejected", bad.value)
			}
		})
	}

	t.Run("org outside namespace", func(t *testing.T) {
		param := validPublishParam()
		param.Org = "huggingface"
		if err := validatePublishParam(param); err == nil {
			t.Fatalf("expected org outside the reserved namespace to be rejected")
		}
	})
	t.Run("invalid repo type", func(t *testing.T) {
		param := validPublishParam()
		param.RepoType = "spaces"
		if err := validatePublishParam(param); err == nil {
			t.Fatalf("expected an invalid repo type to be rejected")
		}
	})
	t.Run("windows reserved name", func(t *testing.T) {
		param := validPublishParam()
		param.Files[0].Path = "nul.txt"
		if err := validatePublishParam(param); err == nil {
			t.Fatalf("expected Windows reserved name to be rejected")
		}
	})
	t.Run("invalid sha256", func(t *testing.T) {
		for _, sha := range []string{"", "not-a-hash", strings.ToUpper(testSha)} {
			param := validPublishParam()
			param.Files[0].Sha256 = sha
			if err := validatePublishParam(param); err == nil {
				t.Fatalf("expected sha256 %q to be rejected", sha)
			}
		}
	})
	t.Run("negative size", func(t *testing.T) {
		param := validPublishParam()
		param.Files[0].Size = -1
		if err := validatePublishParam(param); err == nil {
			t.Fatalf("expected a negative size to be rejected")
		}
	})
	t.Run("size over cache format limit", func(t *testing.T) {
		param := validPublishParam()
		param.Files[0].Size = int64(downloader.DEFAULT_BLOCK_MASK_MAX)*config.SysConfig.Download.BlockSize + 1
		if err := validatePublishParam(param); err == nil {
			t.Fatalf("expected cache format capability limit to be enforced")
		}
	})
}

// §9.4：清单本身的形态问题——空清单、超上限、重复路径。
func TestValidatePublishParamListShape(t *testing.T) {
	withUploadConfig(t, "secret")

	t.Run("empty list", func(t *testing.T) {
		param := validPublishParam()
		param.Files = nil
		if err := validatePublishParam(param); err == nil {
			t.Fatalf("expected an empty publish list to be rejected")
		}
	})
	t.Run("over the file count limit", func(t *testing.T) {
		param := validPublishParam()
		max := config.SysConfig.GetUploadPublishMaxFiles()
		param.Files = make([]dao.LocalManifestFile, 0, max+1)
		for i := 0; i <= max; i++ {
			param.Files = append(param.Files, dao.LocalManifestFile{
				Path: fmt.Sprintf("shard-%04d.bin", i), Sha256: testSha, Size: 1,
			})
		}
		err := validatePublishParam(param)
		if err == nil {
			t.Fatalf("expected the publish list size limit to be enforced")
		}
		if !strings.Contains(err.Error(), "too many files") {
			t.Fatalf("error should explain the limit, got %v", err)
		}
	})
	// 坑 批-5：择一或“后者覆盖前者”会让调用方拿到成功响应，生效的却不是他以为的那个。
	t.Run("duplicate path", func(t *testing.T) {
		param := validPublishParam()
		param.Files = append(param.Files, dao.LocalManifestFile{Path: "config.json", Sha256: testSha, Size: 99})
		err := validatePublishParam(param)
		if err == nil {
			t.Fatalf("expected a duplicate path to be rejected")
		}
		if !strings.Contains(err.Error(), "config.json") {
			t.Fatalf("error should name the duplicate path, got %v", err)
		}
	})
}

// §9.10 凭证：发布接口与上传共用同一个凭证，三类失败必须可区分且不触及落盘。
func TestPublishTokenHandling(t *testing.T) {
	svc := NewUploadService(nil)

	withUploadConfig(t, "")
	if _, err := svc.PublishFiles(validPublishParam(), "anything"); errorCodeOf(err) != "UPLOAD_DISABLED" {
		t.Fatalf("expected UPLOAD_DISABLED when no token is configured, got %v", err)
	}

	withUploadConfig(t, "secret")
	if _, err := svc.PublishFiles(validPublishParam(), ""); errorCodeOf(err) != "UPLOAD_TOKEN_MISSING" {
		t.Fatalf("expected UPLOAD_TOKEN_MISSING, got %v", err)
	}
	if _, err := svc.PublishFiles(validPublishParam(), "wrong"); errorCodeOf(err) != "UPLOAD_TOKEN_INVALID" {
		t.Fatalf("expected UPLOAD_TOKEN_INVALID, got %v", err)
	}
	// 参数错误必须排在身份校验之后。
	bad := validPublishParam()
	bad.Files = nil
	if _, err := svc.PublishFiles(bad, "wrong"); errorCodeOf(err) != "UPLOAD_TOKEN_INVALID" {
		t.Fatalf("expected token check to precede parameter checks, got %v", err)
	}
	if _, err := svc.PublishFiles(bad, "secret"); errorCodeOf(err) != "PUBLISH_INVALID_ARGUMENT" {
		t.Fatalf("expected PUBLISH_INVALID_ARGUMENT, got %v", err)
	}
}
