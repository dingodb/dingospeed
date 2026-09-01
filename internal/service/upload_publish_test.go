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
	withUploadConfig(t)

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
	withUploadConfig(t)

	// 空清单是合法的：新建仓库/新建 revision 的第一步就是发布一份空清单。
	t.Run("empty list", func(t *testing.T) {
		param := validPublishParam()
		param.Files = nil
		if err := validatePublishParam(param); err != nil {
			t.Fatalf("an empty publish list should be accepted, got %v", err)
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

func validPublishTreeParam() dao.LocalPublishTreeParam {
	return dao.LocalPublishTreeParam{
		RepoType:   "models",
		Org:        "dingo-local",
		Repo:       "demo",
		Revision:   "main",
		BaseCommit: testSha,
		Files: []dao.LocalManifestFile{
			{Path: "config.json", Sha256: testSha, Size: 10},
		},
	}
}

func TestValidatePublishTreeParamRejectionMatrix(t *testing.T) {
	withUploadConfig(t)

	cases := []struct {
		name   string
		mutate func(*dao.LocalPublishTreeParam)
		want   string
	}{
		{"missing base commit", func(p *dao.LocalPublishTreeParam) { p.BaseCommit = "" }, "baseCommit"},
		{"short base commit", func(p *dao.LocalPublishTreeParam) { p.BaseCommit = "abc123" }, "baseCommit"},
		{"uppercase base commit", func(p *dao.LocalPublishTreeParam) { p.BaseCommit = strings.ToUpper(testSha) }, "baseCommit"},
		{"path escape", func(p *dao.LocalPublishTreeParam) {
			p.Files = []dao.LocalManifestFile{{Path: "../escape.bin", Sha256: testSha, Size: 1}}
		}, "filePath"},
		{"duplicate path", func(p *dao.LocalPublishTreeParam) {
			p.Files = []dao.LocalManifestFile{
				{Path: "config.json", Sha256: testSha, Size: 1},
				{Path: "config.json", Sha256: testSha, Size: 2},
			}
		}, "duplicate path"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			param := validPublishTreeParam()
			tc.mutate(&param)
			err := validatePublishTreeParam(param)
			if err == nil {
				t.Fatal("expected the param to be rejected")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err.Error(), tc.want)
			}
		})
	}
}

// 把一个 revision 里的文件删光是一次正常的编辑：标签保留，指向一份空清单，
// 与新建出来还没加文件的 revision 是同一个状态。
func TestValidatePublishTreeParamAcceptsClearingEveryFile(t *testing.T) {
	withUploadConfig(t)

	param := validPublishTreeParam()
	param.Files = nil
	if err := validatePublishTreeParam(param); err != nil {
		t.Fatalf("clearing every file should be accepted, got %v", err)
	}
}

func TestValidatePublishTreeParamAcceptsMetaRevision(t *testing.T) {
	withUploadConfig(t)

	for _, revision := range []string{"meta", "Meta"} {
		param := validPublishTreeParam()
		param.Revision = revision
		if err := validatePublishTreeParam(param); err != nil {
			t.Fatalf("%s must be accepted like an ordinary revision, got %v", revision, err)
		}
	}
}

func TestValidatePublishTreeParamAcceptsAnEdit(t *testing.T) {
	withUploadConfig(t)
	if err := validatePublishTreeParam(validPublishTreeParam()); err != nil {
		t.Fatalf("expected the param to be accepted: %v", err)
	}
}
