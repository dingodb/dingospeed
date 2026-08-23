package service

import (
	"testing"

	"dingospeed/internal/dao"
	"dingospeed/pkg/config"
)

// 服务层只负责鉴权、筛选、排序、分页；磁盘上的判定在 dao 里测。
// 这里用一个空仓库目录就够了——被测的是这几层纯逻辑。

func newCacheAdminServiceForTest(t *testing.T) *CacheAdminService {
	t.Helper()
	oldConfig := config.SysConfig
	config.SysConfig = &config.Config{
		Server:   config.ServerConfig{Repos: t.TempDir()},
		Download: config.Download{BlockSize: 1024},
		Upload:   config.Upload{Namespace: "dingo-local"},
	}
	t.Cleanup(func() { config.SysConfig = oldConfig })
	return NewCacheAdminService(dao.NewCacheAdminDao(nil))
}

// 缓存管理接口会删数据，空批次必须被明确拒绝，不能被当成"删全部"。
func TestCacheAdminRejectsEmptyDeleteBatch(t *testing.T) {
	svc := newCacheAdminServiceForTest(t)
	if _, err := svc.SoftDelete(nil); err == nil {
		t.Fatalf("an empty delete batch must be rejected")
	}
	if _, err := svc.PurgeOrphans(nil); err == nil {
		t.Fatalf("an empty purge batch must be rejected")
	}
}

func rowsOf(paths ...string) []*dao.CacheFileRow {
	rows := make([]*dao.CacheFileRow, 0, len(paths))
	for i, path := range paths {
		rows = append(rows, &dao.CacheFileRow{Path: path, Size: int64(i), BlobMTime: int64(i)})
	}
	return rows
}

func TestSortFileRowsByEachKey(t *testing.T) {
	rows := rowsOf("c", "a", "b")
	sortFileRows(rows, "path", "asc")
	if rows[0].Path != "a" || rows[2].Path != "c" {
		t.Fatalf("ascending sort by path failed: %v", pathsOf(rows))
	}
	sortFileRows(rows, "path", "desc")
	if rows[0].Path != "c" || rows[2].Path != "a" {
		t.Fatalf("descending sort by path failed: %v", pathsOf(rows))
	}
	sortFileRows(rows, "size", "desc")
	if rows[0].Size < rows[2].Size {
		t.Fatalf("descending sort by size failed")
	}
	sortFileRows(rows, "time", "asc")
	if rows[0].BlobMTime > rows[2].BlobMTime {
		t.Fatalf("ascending sort by time failed")
	}
}

func pathsOf(rows []*dao.CacheFileRow) []string {
	result := make([]string, 0, len(rows))
	for _, row := range rows {
		result = append(result, row.Path)
	}
	return result
}

func TestPaginateClampsPage(t *testing.T) {
	rows := rowsOf("a", "b", "c", "d", "e")

	page, got := paginate(rows, CacheQuery{Page: 1, PageSize: 2})
	if page != 1 || len(got) != 2 || got[0].Path != "a" {
		t.Fatalf("first page = %d %v", page, pathsOf(got))
	}
	page, got = paginate(rows, CacheQuery{Page: 3, PageSize: 2})
	if page != 3 || len(got) != 1 || got[0].Path != "e" {
		t.Fatalf("last page = %d %v", page, pathsOf(got))
	}
	// 删完最后一页之后前端不刷新页码也要看得到东西，所以越界回落到最后一页而不是空列表。
	page, got = paginate(rows, CacheQuery{Page: 99, PageSize: 2})
	if page != 3 || len(got) != 1 {
		t.Fatalf("out-of-range page must fall back to the last page, got page=%d rows=%v", page, pathsOf(got))
	}
	page, got = paginate(rows, CacheQuery{Page: 0, PageSize: 0})
	if page != 1 || len(got) != 5 {
		t.Fatalf("defaults must return the whole small set, got page=%d rows=%v", page, pathsOf(got))
	}
	// 空结果集不能算出第 0 页，前端会显示“第 0 / 0 页”。
	page, got = paginate([]*dao.CacheFileRow{}, CacheQuery{Page: 1, PageSize: 10})
	if page != 1 || len(got) != 0 {
		t.Fatalf("empty result = page %d rows %v", page, pathsOf(got))
	}
}

func TestPaginateCapsPageSize(t *testing.T) {
	rows := make([]*dao.CacheFileRow, 1200)
	for i := range rows {
		rows[i] = &dao.CacheFileRow{}
	}
	_, got := paginate(rows, CacheQuery{Page: 1, PageSize: 100000})
	if len(got) != 500 {
		t.Fatalf("page size must be capped at 500, got %d", len(got))
	}
}

func TestMatchKeywordIsCaseInsensitiveAcrossFields(t *testing.T) {
	if !matchKeyword("", "anything") {
		t.Fatalf("an empty keyword must match everything")
	}
	if !matchKeyword("MODEL", "weights/model.bin", "abc", "org/repo") {
		t.Fatalf("keyword matching must be case insensitive")
	}
	if !matchKeyword("org/", "weights/model.bin", "abc", "org/repo") {
		t.Fatalf("keyword must also match the repo field")
	}
	if matchKeyword("missing", "weights/model.bin", "abc", "org/repo") {
		t.Fatalf("unrelated keyword must not match")
	}
}

// 列表页脚的合计与汇总卡的"缓存内容大小"必须是同一个口径。
// 现网出过一次：一份 1.4GB 的缓存在列表里显示成 20GB——一行是 (path, sha)，
// 同一个 blob 被 14 个路径引用就被算了 14 遍。这个数紧挨着删除按钮，
// 会被读成"删完能释放多少"。
func TestFileTotalSizeCountsEachBlobOnce(t *testing.T) {
	rows := []*dao.CacheFileRow{
		// 同一个 blob，仓库内三个路径引用 → 只能算一次。
		{RepoType: "models", OrgRepo: "dingo-local/demo", Path: "a.bin", Sha: "sha1", Size: 100},
		{RepoType: "models", OrgRepo: "dingo-local/demo", Path: "b.bin", Sha: "sha1", Size: 100},
		{RepoType: "models", OrgRepo: "dingo-local/demo", Path: "sub/c.bin", Sha: "sha1", Size: 100},
		// 同仓库的另一份内容 → 单独计。
		{RepoType: "models", OrgRepo: "dingo-local/demo", Path: "d.bin", Sha: "sha2", Size: 30},
		// 同一个 sha 落在另一个仓库：blob 路径含 org/repo，盘上是两个文件，必须重复计。
		{RepoType: "models", OrgRepo: "Qwen/Qwen2.5", Path: "a.bin", Sha: "sha1", Size: 100},
		// repoType 也参与身份。
		{RepoType: "datasets", OrgRepo: "dingo-local/demo", Path: "a.bin", Sha: "sha1", Size: 100},
	}
	if got := sumDistinctBlobs(rows); got != 330 {
		t.Fatalf("total = %d, want 330 (100+30 一个仓库 + 100 + 100 另两个)", got)
	}
	if got := sumDistinctBlobs(nil); got != 0 {
		t.Fatalf("empty total = %d, want 0", got)
	}
}
