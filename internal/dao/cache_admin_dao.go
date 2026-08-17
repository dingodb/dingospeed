//  Copyright (c) 2025 dingodb.com, Inc. All Rights Reserved
//
//  Licensed under the Apache License, Version 2.0 (the "License");
//  you may not use this file except in compliance with the License.
//  You may obtain a copy of the License at
//
//      http:www.apache.org/licenses/LICENSE-2.0
//
//  Unless required by applicable law or agreed to in writing, software
//  distributed under the License is distributed on an "AS IS" BASIS,
//  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//  See the License for the specific language governing permissions and
//  limitations under the License.

package dao

import (
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"dingospeed/internal/downloader"
	"dingospeed/pkg/common"
	"dingospeed/pkg/config"
	"dingospeed/pkg/consts"
	"dingospeed/pkg/util"

	"github.com/bytedance/sonic"
	"go.uber.org/zap"
)

// 缓存管理的两级删除。
//
// 一级删除（页面上叫“删除”）只摘除引用：远端缓存删 paths-info + resolve 链接，
// 本地上传从该仓库**全部**快照清单里摘掉这条记录。内容本身（blobs/<sha>）原封不动，
// 同时留下一份回收站墓碑，记下“最后一个引用是什么时候没的”。
//
// 二级删除（页面上叫“彻底删除”）才真正 os.Remove 掉 blob。
//
// 为什么本地上传不能只删 resolve：清单才是本地仓库的权威引用（见 UploadDao 的注释），
// resolve 链接是下载链路懒建立的派生物，删掉之后清单仍然引用着 blob，
// CleanupUnreferencedBlobs 永远不会回收它，而下一次下载还会把链接重建出来。
const recycleDirName = "recycle"

// 内容来源。upload 指本地上传命名空间下的自研内容，remote 指从上游镜像拉取的缓存。
const (
	CacheSourceUpload = "upload"
	CacheSourceRemote = "remote"
)

type CacheAdminDao struct {
	fileDao *FileDao
}

func NewCacheAdminDao(fileDao *FileDao) *CacheAdminDao {
	return &CacheAdminDao{fileDao: fileDao}
}

// CacheRepo 是仓库维度的汇总，供页面左侧的仓库树使用。
type CacheRepo struct {
	RepoType  string `json:"repoType"`
	OrgRepo   string `json:"orgRepo"`
	Source    string `json:"source"`
	FileCount int    `json:"fileCount"`
	TotalSize int64  `json:"totalSize"`
	// OrphanCount/OrphanSize 是该仓库已进回收站的条目，便于在树上直接看到哪儿有可释放空间。
	OrphanCount int   `json:"orphanCount"`
	OrphanSize  int64 `json:"orphanSize"`
}

// CacheFileRow 是一级列表的一行：仓库内某个路径的某一份内容。
//
// 同一个路径在不同快照里可能是不同内容（改过又发布），那是两行；
// 同一份内容在同一个快照里被多个路径引用，那也是两行。行的身份是 (path, sha)，
// 与一级删除的粒度严格一致。
type CacheFileRow struct {
	RepoType    string   `json:"repoType"`
	OrgRepo     string   `json:"orgRepo"`
	Path        string   `json:"path"`
	Sha         string   `json:"sha"`
	Size        int64    `json:"size"`
	CachedBytes int64    `json:"cachedBytes"`
	Complete    bool     `json:"complete"`
	Source      string   `json:"source"`
	Commits     []string `json:"commits"`
	Revisions   []string `json:"revisions"`
	BlobMTime   int64    `json:"blobMTime"`
	BlobExists  bool     `json:"blobExists"`
	HasResolve  bool     `json:"hasResolve"`
}

// RecycleEntry 是回收站墓碑，落在 repos/api/<repoType>/<orgRepo>/recycle/<sha>.json。
//
// 放在 api 下而不是 files 下是有意的：diskClean 的 LRU 只扫 repos/files
// （SysService.checkDiskUsage），墓碑放在那儿会被它当成普通缓存文件删掉，
// 回收站的保留期就没人记得了。
//
// 一个 sha 一个文件，而不是每个仓库一份索引：并发删除各写各的文件，
// 不需要读改写合并，天然原子。
type RecycleEntry struct {
	RepoType  string   `json:"repoType"`
	OrgRepo   string   `json:"orgRepo"`
	Sha       string   `json:"sha"`
	Size      int64    `json:"size"`
	Source    string   `json:"source"`
	Paths     []string `json:"paths"`
	Revisions []string `json:"revisions"`
	// UnlinkedAt 是最后一个引用被删除的时刻（秒）。保留期从这里开始算，
	// 而不是从 blob 的 mtime 算——三个月前上传的文件今天删，mtime 早就过期了。
	UnlinkedAt int64 `json:"unlinkedAt"`
}

// RecycleRow 是二级列表（回收站）的一行。
type RecycleRow struct {
	RecycleEntry
	DiskSize  int64 `json:"diskSize"`
	ExpiresAt int64 `json:"expiresAt"`
	// Inferred 为 true 表示这条没有墓碑，是扫描时按“清单未引用”推断出来的历史残留
	// （批量发布留下的暂缓生效内容）。它的 UnlinkedAt 取 blob mtime，只能算个近似。
	Inferred bool `json:"inferred"`
}

// DeleteItem 是删除请求里的一条。二级删除只用 RepoType/OrgRepo/Sha。
type DeleteItem struct {
	RepoType string `json:"repoType"`
	OrgRepo  string `json:"orgRepo"`
	Path     string `json:"path"`
	Sha      string `json:"sha"`
}

// DeleteResult 逐条返回，不因为其中一条失败就整批回滚：
// 删除是幂等的，调用方重试即可，而整批回滚反而要把已经摘掉的引用再加回去。
type DeleteResult struct {
	DeleteItem
	Status string `json:"status"` // deleted | skipped | failed
	Reason string `json:"reason,omitempty"`
}

// ---------------------------------------------------------------------------
// blob 元信息
// ---------------------------------------------------------------------------

type blobStat struct {
	// Size 是内容的真实字节数，取自 DingCache 头部，不是磁盘上的文件大小。
	Size        int64
	CachedBytes int64
	Complete    bool
	DiskSize    int64
	ModTime     time.Time
	Exists      bool
}

// readBlobStat 以只读方式解析 blob。
//
// 不能用 NewDingCache：它以 O_RDWR 打开，头部读不出来时会**写**一个新头部覆盖掉，
// 文件不存在时还会把文件创建出来。查看页面扫过一遍就把缓存改了，那是不能接受的。
func readBlobStat(path string) blobStat {
	info, err := os.Lstat(path)
	if err != nil || info.IsDir() {
		return blobStat{}
	}
	stat := blobStat{DiskSize: info.Size(), ModTime: info.ModTime(), Exists: true}
	f, err := os.Open(path)
	if err != nil {
		return stat
	}
	defer f.Close()
	header := &downloader.DingCacheHeader{}
	if err = header.Read(f); err != nil {
		// 不是 DingCache 格式（老数据迁移残留、或降级成拷贝的普通文件），按普通文件看待。
		stat.Size = info.Size()
		stat.CachedBytes = info.Size()
		stat.Complete = true
		return stat
	}
	stat.Size = int64(header.FileSize)
	if header.BlockSize == 0 {
		return stat
	}
	blockNum := int64(header.BlockNumber)
	var cached int64
	complete := true
	for i := int64(0); i < blockNum; i++ {
		has, testErr := header.BlockMask.Test(uint64(i))
		if testErr != nil {
			complete = false
			break
		}
		if has {
			cached += int64(header.BlockSize)
		} else {
			complete = false
		}
	}
	if cached > stat.Size {
		cached = stat.Size
	}
	stat.CachedBytes = cached
	stat.Complete = complete && stat.Size >= 0
	return stat
}

// ---------------------------------------------------------------------------
// 仓库索引
// ---------------------------------------------------------------------------

type repoRef struct {
	Commit string
	Path   string
}

// repoIndex 是一个仓库的完整快照：盘上有哪些 blob、每个 blob 被哪些 (快照, 路径) 引用。
// 一级列表、引用判定、回收站列表全部从它派生，保证三者对同一份磁盘状态的解释一致。
type repoIndex struct {
	RepoType string
	OrgRepo  string
	Source   string
	Blobs    map[string]blobStat
	BySha    map[string][]repoRef
	// TagsOf 把快照标识映射回指向它的版本标签（main、v1 之类）。
	TagsOf map[string][]string
}

func repoSource(orgRepo string) string {
	if IsLocalOrgRepo(orgRepo) {
		return CacheSourceUpload
	}
	return CacheSourceRemote
}

func repoFilesRoot(repoType, orgRepo string) string {
	return filepath.Join(config.SysConfig.Repos(), "files", repoType, filepath.FromSlash(orgRepo))
}

func repoApiRoot(repoType, orgRepo string) string {
	return filepath.Join(config.SysConfig.Repos(), "api", repoType, filepath.FromSlash(orgRepo))
}

func recycleEntryPath(repoType, orgRepo, sha string) string {
	return filepath.Join(repoApiRoot(repoType, orgRepo), recycleDirName, sha+".json")
}

// buildRepoIndex 是包级函数而不是方法：自动回收任务（UploadDao）与管理接口
// （CacheAdminDao）必须用同一套引用判定，否则页面显示“已进回收站”而后台认为仍被引用，
// 或者反过来后台删掉了页面上还在用的内容。
func buildRepoIndex(repoType, orgRepo string) *repoIndex {
	idx := &repoIndex{
		RepoType: repoType,
		OrgRepo:  orgRepo,
		Source:   repoSource(orgRepo),
		Blobs:    make(map[string]blobStat),
		BySha:    make(map[string][]repoRef),
		TagsOf:   make(map[string][]string),
	}
	collectRepoBlobs(idx)
	collectRepoTags(idx)
	if idx.Source == CacheSourceUpload {
		collectManifestRefs(idx)
	} else {
		collectRemoteRefs(idx)
	}
	return idx
}

func collectRepoBlobs(idx *repoIndex) {
	blobsDir := filepath.Join(repoFilesRoot(idx.RepoType, idx.OrgRepo), "blobs")
	entries, err := os.ReadDir(blobsDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || strings.HasSuffix(entry.Name(), localUploadStageSuffix) {
			continue
		}
		stat := readBlobStat(filepath.Join(blobsDir, entry.Name()))
		if stat.Exists {
			idx.Blobs[entry.Name()] = stat
		}
	}
}

// collectRepoTags 读 revision/<tag>/meta_get.json 里的 sha，建立 快照 → 标签 的反向映射。
// 上传侧与下载侧写的是同一种 CacheContent 格式，所以这段对两类仓库通用。
func collectRepoTags(idx *repoIndex) {
	revisionRoot := filepath.Join(repoApiRoot(idx.RepoType, idx.OrgRepo), "revision")
	entries, err := os.ReadDir(revisionRoot)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		sha := readMetaSha(filepath.Join(revisionRoot, entry.Name(), "meta_get.json"))
		if sha == "" || sha == entry.Name() {
			continue
		}
		idx.TagsOf[sha] = appendUnique(idx.TagsOf[sha], entry.Name())
	}
}

func readMetaSha(metaPath string) string {
	content, err := readCacheContent(metaPath)
	if err != nil {
		return ""
	}
	var meta struct {
		Sha string `json:"sha"`
	}
	if err = sonic.Unmarshal(content, &meta); err != nil {
		return ""
	}
	return meta.Sha
}

// readCacheContent 解出 CacheContent 包装里的原始响应体。
func readCacheContent(path string) ([]byte, error) {
	b, err := util.ReadFileToBytes(path)
	if err != nil {
		return nil, err
	}
	var cacheContent common.CacheContent
	if err = sonic.Unmarshal(b, &cacheContent); err != nil {
		return nil, err
	}
	if cacheContent.Version != consts.VersionSnapshot {
		return nil, fmt.Errorf("unsupported cache content version: %d", cacheContent.Version)
	}
	// 响应体在文件里是十六进制编码的，OriginContent 只是解码后的暂存字段，不落盘。
	return hex.DecodeString(cacheContent.Content)
}

// collectManifestRefs 收集本地仓库的引用：扫全部快照清单，与 referencedShas 同源。
func collectManifestRefs(idx *repoIndex) {
	revisionRoot := filepath.Join(repoApiRoot(idx.RepoType, idx.OrgRepo), "revision")
	entries, err := os.ReadDir(revisionRoot)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		manifest, readErr := readManifestFile(LocalManifestPath(idx.RepoType, idx.OrgRepo, entry.Name()))
		if readErr != nil {
			continue
		}
		for _, item := range manifest {
			idx.BySha[item.Sha256] = append(idx.BySha[item.Sha256], repoRef{Commit: entry.Name(), Path: item.Path})
		}
	}
}

// collectRemoteRefs 收集远端仓库的引用。
//
// 以 paths-info 为主、resolve 链接为辅，而不是只 readlink：
// CreateLinkOrCopyIfNotExists 是 软链 → 硬链 → 拷贝 三级降级，
// Windows 上拿不到软链时 Readlink 直接失败，光靠它会把整仓文件都判成“无法识别”。
// 反过来 paths-info 也可能被 diskClean 之外的原因缺失，所以两边取并集。
func collectRemoteRefs(idx *repoIndex) {
	seen := make(map[string]struct{})
	addRef := func(sha, commit, path string) {
		if sha == "" {
			return
		}
		key := sha + "\x00" + commit + "\x00" + path
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		idx.BySha[sha] = append(idx.BySha[sha], repoRef{Commit: commit, Path: path})
	}

	pathsInfoRoot := filepath.Join(repoApiRoot(idx.RepoType, idx.OrgRepo), "paths-info")
	_ = filepath.WalkDir(pathsInfoRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || entry.Name() != "paths-info_post.json" {
			return nil
		}
		rel, relErr := filepath.Rel(pathsInfoRoot, filepath.Dir(path))
		if relErr != nil {
			return nil
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) < 2 {
			return nil
		}
		addRef(readPathsInfoOid(path), parts[0], strings.Join(parts[1:], "/"))
		return nil
	})

	resolveRoot := filepath.Join(repoFilesRoot(idx.RepoType, idx.OrgRepo), "resolve")
	_ = filepath.WalkDir(resolveRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(resolveRoot, path)
		if relErr != nil {
			return nil
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) < 2 {
			return nil
		}
		addRef(resolveLinkSha(path, idx.Blobs), parts[0], strings.Join(parts[1:], "/"))
		return nil
	})
}

func readPathsInfoOid(path string) string {
	content, err := readCacheContent(path)
	if err != nil {
		return ""
	}
	pathsInfos := make([]common.PathsInfo, 0)
	if err = sonic.Unmarshal(content, &pathsInfos); err != nil || len(pathsInfos) == 0 {
		return ""
	}
	if pathsInfos[0].Lfs.Oid != "" {
		return pathsInfos[0].Lfs.Oid
	}
	return pathsInfos[0].Oid
}

// resolveLinkSha 尽力从一个 resolve 条目反查它指向哪个 blob。
// 软链读链接目标；硬链/拷贝读不出来，只能靠“大小与 mtime 都对得上”去猜，
// 猜不中就返回空——返回空只会让这条 resolve 不算作引用，由 paths-info 那一路兜底。
func resolveLinkSha(path string, blobs map[string]blobStat) string {
	if target, err := os.Readlink(path); err == nil {
		return filepath.Base(target)
	}
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	match := ""
	for sha, stat := range blobs {
		if stat.DiskSize != info.Size() || !stat.ModTime.Equal(info.ModTime()) {
			continue
		}
		if match != "" {
			return "" // 多个候选，宁可不认，也不要认错
		}
		match = sha
	}
	return match
}

func readManifestFile(path string) ([]LocalManifestFile, error) {
	b, err := util.ReadFileToBytes(path)
	if err != nil {
		return nil, err
	}
	var manifest []LocalManifestFile
	if err = sonic.Unmarshal(b, &manifest); err != nil {
		return nil, err
	}
	return manifest, nil
}

func appendUnique(list []string, item string) []string {
	for _, v := range list {
		if v == item {
			return list
		}
	}
	return append(list, item)
}

// ---------------------------------------------------------------------------
// 仓库枚举
// ---------------------------------------------------------------------------

type repoKey struct {
	RepoType string
	OrgRepo  string
}

// listRepoKeys 枚举所有仓库。
//
// orgRepo 的层级不固定（有 org 时是 org/repo，没有时就是 repo），所以不能按固定深度
// glob，只能向下找到“含 blobs 或 resolve 子目录”的那一层作为仓库根，找到就不再深入。
func listRepoKeys() []repoKey {
	keys := make(map[repoKey]struct{})
	repos := config.SysConfig.Repos()
	for _, repoType := range []string{"models", "datasets", "spaces"} {
		scanRepoRoots(filepath.Join(repos, "files", repoType), repoType, []string{"blobs", "resolve"}, keys)
		scanRepoRoots(filepath.Join(repos, "api", repoType), repoType, []string{"revision"}, keys)
	}
	result := make([]repoKey, 0, len(keys))
	for k := range keys {
		result = append(result, k)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].RepoType != result[j].RepoType {
			return result[i].RepoType < result[j].RepoType
		}
		return result[i].OrgRepo < result[j].OrgRepo
	})
	return result
}

func scanRepoRoots(root, repoType string, markers []string, out map[repoKey]struct{}) {
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || !entry.IsDir() || path == root {
			return nil
		}
		for _, marker := range markers {
			if entry.Name() == marker {
				// 命中标记目录，它的父目录就是仓库根。
				rel, relErr := filepath.Rel(root, filepath.Dir(path))
				if relErr == nil && rel != "." && !strings.HasPrefix(rel, "..") {
					out[repoKey{RepoType: repoType, OrgRepo: filepath.ToSlash(rel)}] = struct{}{}
				}
				return fs.SkipDir
			}
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// 一级列表
// ---------------------------------------------------------------------------

func (d *CacheAdminDao) ListRepos() []*CacheRepo {
	result := make([]*CacheRepo, 0)
	for _, key := range listRepoKeys() {
		idx := buildRepoIndex(key.RepoType, key.OrgRepo)
		rows := indexRows(idx)
		repo := &CacheRepo{
			RepoType:  key.RepoType,
			OrgRepo:   key.OrgRepo,
			Source:    idx.Source,
			FileCount: len(rows),
		}
		counted := make(map[string]struct{})
		for _, row := range rows {
			if _, ok := counted[row.Sha]; ok {
				continue
			}
			counted[row.Sha] = struct{}{}
			repo.TotalSize += row.Size
		}
		for _, orphan := range indexOrphans(idx) {
			repo.OrphanCount++
			repo.OrphanSize += orphan.Size
		}
		if repo.FileCount == 0 && repo.OrphanCount == 0 {
			continue
		}
		result = append(result, repo)
	}
	return result
}

// ListFiles 返回某个仓库的一级列表；orgRepo 为空时返回全部仓库的合集。
func (d *CacheAdminDao) ListFiles(repoType, orgRepo string) []*CacheFileRow {
	rows := make([]*CacheFileRow, 0)
	for _, key := range listRepoKeys() {
		if repoType != "" && key.RepoType != repoType {
			continue
		}
		if orgRepo != "" && key.OrgRepo != orgRepo {
			continue
		}
		rows = append(rows, indexRows(buildRepoIndex(key.RepoType, key.OrgRepo))...)
	}
	return rows
}

func indexRows(idx *repoIndex) []*CacheFileRow {
	byKey := make(map[string]*CacheFileRow)
	order := make([]string, 0)
	for sha, refs := range idx.BySha {
		stat := idx.Blobs[sha]
		for _, ref := range refs {
			key := ref.Path + "\x00" + sha
			row, ok := byKey[key]
			if !ok {
				row = &CacheFileRow{
					RepoType:    idx.RepoType,
					OrgRepo:     idx.OrgRepo,
					Path:        ref.Path,
					Sha:         sha,
					Size:        stat.Size,
					CachedBytes: stat.CachedBytes,
					Complete:    stat.Complete,
					Source:      idx.Source,
					BlobExists:  stat.Exists,
					Commits:     make([]string, 0, 1),
					Revisions:   make([]string, 0, 1),
				}
				if stat.Exists {
					row.BlobMTime = stat.ModTime.Unix()
				}
				byKey[key] = row
				order = append(order, key)
			}
			row.Commits = appendUnique(row.Commits, ref.Commit)
			for _, tag := range idx.TagsOf[ref.Commit] {
				row.Revisions = appendUnique(row.Revisions, tag)
			}
			if !row.HasResolve && util.FileExists(ResolvePath(idx.RepoType, idx.OrgRepo, ref.Commit, ref.Path)) {
				row.HasResolve = true
			}
		}
	}
	rows := make([]*CacheFileRow, 0, len(order))
	for _, key := range order {
		row := byKey[key]
		sort.Strings(row.Commits)
		sort.Strings(row.Revisions)
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Path != rows[j].Path {
			return rows[i].Path < rows[j].Path
		}
		return rows[i].Sha < rows[j].Sha
	})
	return rows
}

// ---------------------------------------------------------------------------
// 回收站列表
// ---------------------------------------------------------------------------

func (d *CacheAdminDao) ListOrphans(repoType, orgRepo string) []*RecycleRow {
	rows := make([]*RecycleRow, 0)
	for _, key := range listRepoKeys() {
		if repoType != "" && key.RepoType != repoType {
			continue
		}
		if orgRepo != "" && key.OrgRepo != orgRepo {
			continue
		}
		rows = append(rows, indexOrphans(buildRepoIndex(key.RepoType, key.OrgRepo))...)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].UnlinkedAt < rows[j].UnlinkedAt })
	return rows
}

// indexOrphans 列出该仓库下“盘上还在、但已经没有任何引用”的内容。
//
// 两个来源合并：
//  1. 有墓碑的（用户一级删除留下的），保留期从墓碑的 unlinkedAt 起算；
//  2. 本地命名空间下无墓碑但清单未引用的——这正是 CleanupUnreferencedBlobs 一直在回收的
//     那类残留（暂缓生效的上传等不到发布）。把它们一并显示出来，页面上看到的
//     “会被自动删掉的东西”才与后台实际会删的东西一致。它们没有 unlinkedAt 可用，
//     只能回落到 blob mtime，因此标 Inferred。
//
// 远端仓库不做第 2 类推断：远端 blob 没有引用可能只是 resolve 被 diskClean 的 LRU 删了，
// 与用户的删除动作无关，把它算成“待彻底删除”会让页面怂恿用户删掉正常的缓存。
func indexOrphans(idx *repoIndex) []*RecycleRow {
	retention := config.SysConfig.GetUploadOrphanRetention()
	rows := make([]*RecycleRow, 0)
	tombstones := readRecycleEntries(idx.RepoType, idx.OrgRepo)
	for sha, stat := range idx.Blobs {
		if len(idx.BySha[sha]) > 0 {
			continue
		}
		entry, hasTombstone := tombstones[sha]
		if !hasTombstone {
			if idx.Source != CacheSourceUpload {
				continue
			}
			entry = RecycleEntry{
				RepoType:   idx.RepoType,
				OrgRepo:    idx.OrgRepo,
				Sha:        sha,
				Size:       stat.Size,
				Source:     idx.Source,
				Paths:      []string{},
				Revisions:  []string{},
				UnlinkedAt: stat.ModTime.Unix(),
			}
		}
		if entry.Size == 0 {
			entry.Size = stat.Size
		}
		rows = append(rows, &RecycleRow{
			RecycleEntry: entry,
			DiskSize:     stat.DiskSize,
			ExpiresAt:    entry.UnlinkedAt + int64(retention.Seconds()),
			Inferred:     !hasTombstone,
		})
	}
	return rows
}

// readRecycleEntries 读该仓库全部墓碑。
//
// 这里只读不删。引用又回来了的墓碑（重新上传过同一份内容）确实已经作废，但删除它
// 必须在仓库锁下做：读列表是不加锁的，若在这里顺手删，一次与软删除并发的列表请求
// 就可能把刚写下的、完全有效的墓碑抹掉，那条内容的保留期起点就丢了。
// 作废墓碑的清理交给持有仓库锁的两趟回收（CleanupUnreferencedBlobs / CleanupRecycledBlobs），
// 列表这边只要不把它显示出来就够了。
func readRecycleEntries(repoType, orgRepo string) map[string]RecycleEntry {
	result := make(map[string]RecycleEntry)
	dir := filepath.Join(repoApiRoot(repoType, orgRepo), recycleDirName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return result
	}
	for _, item := range entries {
		if item.IsDir() || !strings.HasSuffix(item.Name(), ".json") {
			continue
		}
		b, readErr := util.ReadFileToBytes(filepath.Join(dir, item.Name()))
		if readErr != nil {
			continue
		}
		var entry RecycleEntry
		if err = sonic.Unmarshal(b, &entry); err != nil || entry.Sha == "" {
			continue
		}
		result[entry.Sha] = entry
	}
	return result
}

func writeRecycleEntry(entry RecycleEntry) error {
	path := recycleEntryPath(entry.RepoType, entry.OrgRepo, entry.Sha)
	if err := ensureLocalUploadPathSafe(config.SysConfig.Repos(), path); err != nil {
		return err
	}
	if err := util.MakeDirs(path); err != nil {
		return err
	}
	return util.WriteDataToFileAtomic(path, entry)
}

func removeRecycleEntry(repoType, orgRepo, sha string) {
	if err := os.Remove(recycleEntryPath(repoType, orgRepo, sha)); err != nil && !os.IsNotExist(err) {
		zap.S().Warnf("[CACHE-ADMIN] remove recycle entry failed: %s/%s sha=%s err=%v", repoType, orgRepo, sha, err)
	}
}

// ---------------------------------------------------------------------------
// 一级删除
// ---------------------------------------------------------------------------

func (d *CacheAdminDao) SoftDelete(items []DeleteItem) ([]*DeleteResult, error) {
	grouped := make(map[repoKey][]DeleteItem)
	order := make([]repoKey, 0)
	for _, item := range items {
		key := repoKey{RepoType: item.RepoType, OrgRepo: item.OrgRepo}
		if _, ok := grouped[key]; !ok {
			order = append(order, key)
		}
		grouped[key] = append(grouped[key], item)
	}
	results := make([]*DeleteResult, 0, len(items))
	for _, key := range order {
		results = append(results, d.softDeleteRepo(key, grouped[key])...)
	}
	return results, nil
}

func (d *CacheAdminDao) softDeleteRepo(key repoKey, items []DeleteItem) []*DeleteResult {
	if err := validateRepoKey(key); err != nil {
		return failAll(items, err.Error())
	}
	// 仓库锁与上传/发布互斥，顺序 repo → revision → blob，与上传侧保持一致。
	lockKey := uploadRepoLockKey(key.RepoType, key.OrgRepo)
	uploadRepoLocks.Lock(lockKey)
	defer uploadRepoLocks.Unlock(lockKey)

	idx := buildRepoIndex(key.RepoType, key.OrgRepo)
	results := make([]*DeleteResult, 0, len(items))
	for _, item := range items {
		results = append(results, d.softDeleteOne(idx, item))
	}
	return results
}

func (d *CacheAdminDao) softDeleteOne(idx *repoIndex, item DeleteItem) *DeleteResult {
	result := &DeleteResult{DeleteItem: item, Status: "deleted"}
	if item.Path == "" {
		result.Status = "failed"
		result.Reason = "path is required"
		return result
	}
	refs := idx.BySha[item.Sha]
	matched := make([]repoRef, 0, len(refs))
	for _, ref := range refs {
		if ref.Path == item.Path {
			matched = append(matched, ref)
		}
	}
	if len(matched) == 0 {
		result.Status = "skipped"
		result.Reason = "no reference found"
		return result
	}
	// 版本标签要在动清单之前记下来：摘除会让旧快照连同它的标签映射一起消失，
	// 之后再查就只剩空列表，回收站里就看不出这份内容原本挂在哪个版本上。
	revisions := make([]string, 0)
	for _, ref := range matched {
		for _, tag := range idx.TagsOf[ref.Commit] {
			revisions = appendUnique(revisions, tag)
		}
	}

	if idx.Source == CacheSourceUpload {
		if err := d.removeManifestEntries(idx, item.Path, item.Sha, matched); err != nil {
			result.Status = "failed"
			result.Reason = err.Error()
			return result
		}
	} else {
		removePathsInfo(idx, item.Path, matched)
	}
	for _, ref := range matched {
		removeResolveLink(idx.RepoType, idx.OrgRepo, ref.Commit, ref.Path)
	}

	// 从内存索引里同步摘掉，让同一批里针对同一个 sha 的其它条目看到最新的引用状态。
	remaining := make([]repoRef, 0, len(refs))
	for _, ref := range refs {
		if ref.Path != item.Path {
			remaining = append(remaining, ref)
		}
	}
	if len(remaining) == 0 {
		delete(idx.BySha, item.Sha)
		stat := idx.Blobs[item.Sha]
		paths := []string{item.Path}
		if err := writeRecycleEntry(RecycleEntry{
			RepoType:   idx.RepoType,
			OrgRepo:    idx.OrgRepo,
			Sha:        item.Sha,
			Size:       stat.Size,
			Source:     idx.Source,
			Paths:      paths,
			Revisions:  revisions,
			UnlinkedAt: time.Now().Unix(),
		}); err != nil {
			// 引用已经摘掉了，墓碑没写成只是丢了保留期的起点，不该把整条判成失败。
			zap.S().Warnf("[CACHE-ADMIN] write recycle entry failed: %s/%s sha=%s err=%v", idx.RepoType, idx.OrgRepo, item.Sha, err)
			result.Reason = "moved to recycle bin, but retention start time was not recorded"
		}
	} else {
		idx.BySha[item.Sha] = remaining
		result.Reason = "content is still referenced by other paths"
	}
	zap.S().Infof("[CACHE-ADMIN] soft deleted %s/%s path=%s sha=%s", idx.RepoType, idx.OrgRepo, item.Path, item.Sha)
	return result
}

// removeManifestEntries 从该仓库**全部**快照清单里摘掉 (path, sha) 这条记录。
//
// 只摘当前标签指向的那一份是不够的：referencedShas 扫的是全部快照，
// 任何一份历史清单还留着这条，blob 就永远进不了回收站。
//
// 摘除的方式是“旧快照整体作废、内容落到新标识下”，而不是原地改写旧快照：
// 快照标识是清单内容的确定性摘要，原地改写会让标识与内容对不上号，
// 后果不只是语义不洁——发布路径正是靠“算出来的标识与标签当前指向的相同”
// 来判断这批发布没有变化的，标识一旦说谎，删除后重新发布同一个文件会被
// 误判成“无变化”而直接跳过，文件再也回不来。
//
// 代价是旧标识不复存在：记录过它的客户端会拿到 404，而不是一份内容已经变了的快照。
// 这是“把内容真正删掉”不可回避的前提。
func (d *CacheAdminDao) removeManifestEntries(idx *repoIndex, path, sha string, matched []repoRef) error {
	commits := make([]string, 0, len(matched))
	for _, ref := range matched {
		commits = appendUnique(commits, ref.Commit)
	}
	for _, commit := range commits {
		manifest, err := readManifestFile(LocalManifestPath(idx.RepoType, idx.OrgRepo, commit))
		if err != nil {
			continue
		}
		kept := make([]LocalManifestFile, 0, len(manifest))
		changed := false
		for _, entry := range manifest {
			if entry.Path == path && entry.Sha256 == sha {
				changed = true
				continue
			}
			kept = append(kept, entry)
		}
		if !changed {
			continue
		}
		if err = d.replaceSnapshot(idx, commit, kept); err != nil {
			return err
		}
	}
	return nil
}

// replaceSnapshot 用 kept 这份清单取代 oldCommit 这个快照：
// 新清单落到它自己的标识下，指向旧快照的版本标签改指新标识，旧快照整体删除。
func (d *CacheAdminDao) replaceSnapshot(idx *repoIndex, oldCommit string, kept []LocalManifestFile) error {
	newCommit, err := manifestCommit(kept)
	if err != nil {
		return err
	}
	tags := idx.TagsOf[oldCommit]
	if len(kept) == 0 && len(tags) == 0 {
		// 一个没有标签指向、且已经空掉的历史快照没有任何存在意义，
		// 留一个空清单只会让仓库里堆满空目录。
		return d.dropSnapshot(idx, oldCommit)
	}
	if newCommit != oldCommit {
		manifestPath := LocalManifestPath(idx.RepoType, idx.OrgRepo, newCommit)
		if err = ensureLocalUploadPathSafe(config.SysConfig.Repos(), manifestPath); err != nil {
			return err
		}
		if err = util.MakeDirs(manifestPath); err != nil {
			return err
		}
		if err = util.WriteDataToFileAtomic(manifestPath, kept); err != nil {
			return err
		}
	}
	// 元数据是清单的派生物：漏改的话文件树接口还会把已删的文件列出来。
	// 新标识自己的那一份要写，指向它的标签也要一起改指过去。
	if err = d.writeLocalMeta(idx, newCommit, newCommit, kept); err != nil {
		return err
	}
	for _, tag := range tags {
		if err = d.writeLocalMeta(idx, tag, newCommit, kept); err != nil {
			return err
		}
	}
	if newCommit != oldCommit {
		if err = d.dropSnapshot(idx, oldCommit); err != nil {
			return err
		}
		// 标签的指向变了，索引里也要跟着变，同一批里的后续条目才看得到最新状态。
		for _, tag := range tags {
			idx.TagsOf[newCommit] = appendUnique(idx.TagsOf[newCommit], tag)
		}
		for _, refs := range idx.BySha {
			for i := range refs {
				if refs[i].Commit == oldCommit {
					refs[i].Commit = newCommit
				}
			}
		}
	}
	d.fileDao.InvalidateLocalManifest(idx.RepoType, idx.OrgRepo, newCommit)
	return nil
}

// dropSnapshot 抹掉一个快照：清单、元数据，以及它下面的 resolve 链接。
//
// 链接必须一起删，留着不只是垃圾：软链创建失败时它会降级成硬链
// （CreateLinkOrCopyIfNotExists 的三级降级），硬链还在的话彻底删除 blob 也不会释放磁盘。
func (d *CacheAdminDao) dropSnapshot(idx *repoIndex, commit string) error {
	if err := os.RemoveAll(filepath.Join(repoApiRoot(idx.RepoType, idx.OrgRepo), "revision", commit)); err != nil {
		return err
	}
	if err := os.RemoveAll(filepath.Join(repoFilesRoot(idx.RepoType, idx.OrgRepo), "resolve", commit)); err != nil {
		return err
	}
	delete(idx.TagsOf, commit)
	// 清单本来“同一个 commit 不会再变”，FileDao 因此把它长期缓存；
	// 快照消失之后这份缓存必须清掉，否则下载侧还会读到它。
	d.fileDao.InvalidateLocalManifest(idx.RepoType, idx.OrgRepo, commit)
	return nil
}

func (d *CacheAdminDao) writeLocalMeta(idx *repoIndex, revision, commit string, manifest []LocalManifestFile) error {
	siblings := make([]map[string]string, 0, len(manifest))
	var usedStorage int64
	for _, item := range manifest {
		siblings = append(siblings, map[string]string{"rfilename": item.Path})
		usedStorage += item.Size
	}
	body, err := sonic.Marshal(map[string]interface{}{
		"id":          idx.OrgRepo,
		"sha":         commit,
		"siblings":    siblings,
		"usedStorage": usedStorage,
	})
	if err != nil {
		return err
	}
	headers := map[string]string{
		"content-type":   "application/json",
		"content-length": fmt.Sprintf("%d", len(body)),
		"x-repo-commit":  commit,
	}
	apiDir := filepath.Join(repoApiRoot(idx.RepoType, idx.OrgRepo), "revision", revision)
	for _, name := range []string{"meta_get.json", "meta_head.json"} {
		metaPath := filepath.Join(apiDir, name)
		if err = ensureLocalUploadPathSafe(config.SysConfig.Repos(), metaPath); err != nil {
			return err
		}
		if err = util.MakeDirs(metaPath); err != nil {
			return err
		}
		if err = d.fileDao.WriteCacheRequest(metaPath, http.StatusOK, headers, body); err != nil {
			return err
		}
	}
	return nil
}

func removePathsInfo(idx *repoIndex, path string, matched []repoRef) {
	for _, ref := range matched {
		infoPath := filepath.Join(repoApiRoot(idx.RepoType, idx.OrgRepo), "paths-info", ref.Commit, filepath.FromSlash(path), "paths-info_post.json")
		if err := os.Remove(infoPath); err != nil && !os.IsNotExist(err) {
			zap.S().Warnf("[CACHE-ADMIN] remove paths-info failed: %s err=%v", infoPath, err)
		}
	}
}

// removeResolveLink 删掉仓库内路径上的那个链接条目。
// 软链、硬链、拷贝三种形态在这里都是一次 os.Remove：删的是目录项，不是 blob 本身。
func removeResolveLink(repoType, orgRepo, commit, path string) {
	target := ResolvePath(repoType, orgRepo, commit, path)
	if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
		zap.S().Warnf("[CACHE-ADMIN] remove resolve link failed: %s err=%v", target, err)
	}
}

// ---------------------------------------------------------------------------
// 二级删除
// ---------------------------------------------------------------------------

func (d *CacheAdminDao) PurgeOrphans(items []DeleteItem) ([]*DeleteResult, error) {
	results := make([]*DeleteResult, 0, len(items))
	for _, item := range items {
		results = append(results, d.purgeOne(item))
	}
	return results, nil
}

func (d *CacheAdminDao) purgeOne(item DeleteItem) *DeleteResult {
	result := &DeleteResult{DeleteItem: item, Status: "deleted"}
	key := repoKey{RepoType: item.RepoType, OrgRepo: item.OrgRepo}
	if err := validateRepoKey(key); err != nil {
		result.Status = "failed"
		result.Reason = err.Error()
		return result
	}
	if item.Sha == "" {
		result.Status = "failed"
		result.Reason = "sha is required"
		return result
	}
	lockKey := uploadRepoLockKey(item.RepoType, item.OrgRepo)
	uploadRepoLocks.Lock(lockKey)
	defer uploadRepoLocks.Unlock(lockKey)

	// 重新算一遍引用而不是信任前端传来的状态：从页面加载到点确认之间，
	// 完全可能有一次发布或下载把这份内容重新引用起来。
	idx := buildRepoIndex(item.RepoType, item.OrgRepo)
	if len(idx.BySha[item.Sha]) > 0 {
		removeRecycleEntry(item.RepoType, item.OrgRepo, item.Sha)
		result.Status = "skipped"
		result.Reason = "content has been referenced again"
		return result
	}
	if _, ok := idx.Blobs[item.Sha]; !ok {
		removeRecycleEntry(item.RepoType, item.OrgRepo, item.Sha)
		result.Status = "skipped"
		result.Reason = "content no longer exists"
		return result
	}
	if err := reclaimBlobFile(item.RepoType, item.OrgRepo, item.Sha); err != nil {
		result.Status = "failed"
		result.Reason = err.Error()
		return result
	}
	removeRecycleEntry(item.RepoType, item.OrgRepo, item.Sha)
	zap.S().Infof("[CACHE-ADMIN] purged %s/%s sha=%s", item.RepoType, item.OrgRepo, item.Sha)
	return result
}

// reclaimBlobFile 是彻底删除 blob 的唯一入口：手动的二级删除与自动回收都走这里。
//
// 取的是 blob 写锁而不是读锁：os.Remove 与老接口的 os.Rename 一样是“整体消灭这个文件”
// 的动作，必须与正在写入的分块上传互斥。Linux 上少了这条，unlink 之后分块 writer
// 会继续往孤儿 inode 上写，位图更新全部丢失且无人知晓。
func reclaimBlobFile(repoType, orgRepo, sha string) error {
	blobPath := localBlobPath(repoType, orgRepo, sha)
	blobKey := uploadBlobLockKey(repoType, orgRepo, sha)
	uploadBlobLocks.Lock(blobKey)
	defer uploadBlobLocks.Unlock(blobKey)
	// 拿到写锁之后再查句柄：分块上传在写锁下已经被挡住了，这里挡的是**下载**——
	// 下载链路不走 blob 锁，只在 DingCacheManager 里登记引用计数。
	if downloader.GetInstance().IsInUse(blobPath) {
		return fmt.Errorf("content is in use by a running transfer")
	}
	if err := os.Remove(blobPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func validateRepoKey(key repoKey) error {
	if key.RepoType != "models" && key.RepoType != "datasets" && key.RepoType != "spaces" {
		return fmt.Errorf("invalid repoType: %s", key.RepoType)
	}
	if key.OrgRepo == "" {
		return fmt.Errorf("orgRepo is required")
	}
	// orgRepo 直接参与拼路径，必须挡住 .. 与绝对路径。
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(key.OrgRepo)))
	if clean != key.OrgRepo || strings.HasPrefix(clean, "..") || strings.HasPrefix(clean, "/") || filepath.IsAbs(clean) {
		return fmt.Errorf("invalid orgRepo: %s", key.OrgRepo)
	}
	return nil
}

func failAll(items []DeleteItem, reason string) []*DeleteResult {
	results := make([]*DeleteResult, 0, len(items))
	for _, item := range items {
		results = append(results, &DeleteResult{DeleteItem: item, Status: "failed", Reason: reason})
	}
	return results
}
