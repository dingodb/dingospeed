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

package service

import (
	"sort"
	"strings"

	"dingospeed/internal/dao"
	"dingospeed/pkg/config"
)

// CacheAdminService 是缓存管理的应用层：鉴权、筛选、排序、分页。
// 磁盘上的判定与改动一律在 dao 里做，这里不碰文件系统。
type CacheAdminService struct {
	cacheAdminDao *dao.CacheAdminDao
}

func NewCacheAdminService(cacheAdminDao *dao.CacheAdminDao) *CacheAdminService {
	return &CacheAdminService{cacheAdminDao: cacheAdminDao}
}

// CacheQuery 是两个列表接口共用的查询条件。
type CacheQuery struct {
	RepoType string
	OrgRepo  string
	Source   string
	Keyword  string
	Sort     string
	Order    string
	Page     int
	PageSize int
}

type CacheSummary struct {
	RepoCount    int   `json:"repoCount"`
	FileCount    int   `json:"fileCount"`
	TotalSize    int64 `json:"totalSize"`
	DiskSize     int64 `json:"diskSize"`
	OrphanCount  int   `json:"orphanCount"`
	OrphanSize   int64 `json:"orphanSize"`
	UploadCount  int   `json:"uploadCount"`
	RemoteCount  int   `json:"remoteCount"`
	RetentionHrs int   `json:"retentionHours"`
	// CleanupIntervalMinutes 是自动清理的轮询间隔。页面用它说明“过期后最迟多久真正消失”。
	CleanupIntervalMinutes int `json:"cleanupIntervalMinutes"`
}

type CacheFilePage struct {
	Total int                 `json:"total"`
	Page  int                 `json:"page"`
	Rows  []*dao.CacheFileRow `json:"rows"`
	// TotalSize 是**筛选后**全部行的合计，不只是当前页，页面靠它显示“这次筛出多少数据”。
	TotalSize int64 `json:"totalSize"`
}

type RecyclePage struct {
	Total     int               `json:"total"`
	Page      int               `json:"page"`
	Rows      []*dao.RecycleRow `json:"rows"`
	TotalSize int64             `json:"totalSize"`
}

func (s *CacheAdminService) Summary() (*CacheSummary, error) {
	repos := s.cacheAdminDao.ListRepos()
	summary := &CacheSummary{
		RepoCount:              len(repos),
		RetentionHrs:           int(config.SysConfig.GetUploadOrphanRetention().Hours()),
		CleanupIntervalMinutes: int(config.SysConfig.GetUploadStagingCleanupInterval().Minutes()),
	}
	for _, repo := range repos {
		summary.FileCount += repo.FileCount
		summary.TotalSize += repo.TotalSize
		summary.OrphanCount += repo.OrphanCount
		summary.OrphanSize += repo.OrphanSize
		if repo.Source == dao.CacheSourceUpload {
			summary.UploadCount++
		} else {
			summary.RemoteCount++
		}
	}
	summary.DiskSize = summary.TotalSize + summary.OrphanSize
	return summary, nil
}

func (s *CacheAdminService) ListRepos() ([]*dao.CacheRepo, error) {
	return s.cacheAdminDao.ListRepos(), nil
}

func (s *CacheAdminService) ListFiles(query CacheQuery) (*CacheFilePage, error) {
	rows := s.cacheAdminDao.ListFiles(query.RepoType, query.OrgRepo)
	filtered := make([]*dao.CacheFileRow, 0, len(rows))
	for _, row := range rows {
		if query.Source != "" && row.Source != query.Source {
			continue
		}
		if !matchKeyword(query.Keyword, row.Path, row.Sha, row.OrgRepo) {
			continue
		}
		filtered = append(filtered, row)
	}
	totalSize := sumDistinctBlobs(filtered)
	sortFileRows(filtered, query.Sort, query.Order)
	page, pageRows := paginate(filtered, query)
	return &CacheFilePage{
		Total:     len(filtered),
		Page:      page,
		Rows:      pageRows,
		TotalSize: totalSize,
	}, nil
}

func (s *CacheAdminService) ListOrphans(query CacheQuery) (*RecyclePage, error) {
	rows := s.cacheAdminDao.ListOrphans(query.RepoType, query.OrgRepo)
	filtered := make([]*dao.RecycleRow, 0, len(rows))
	var totalSize int64
	for _, row := range rows {
		if query.Source != "" && row.Source != query.Source {
			continue
		}
		if !matchKeyword(query.Keyword, strings.Join(row.Paths, " "), row.Sha, row.OrgRepo) {
			continue
		}
		filtered = append(filtered, row)
		totalSize += row.Size
	}
	sortRecycleRows(filtered, query.Sort, query.Order)
	page, pageRows := paginate(filtered, query)
	return &RecyclePage{
		Total:     len(filtered),
		Page:      page,
		Rows:      pageRows,
		TotalSize: totalSize,
	}, nil
}

func (s *CacheAdminService) SoftDelete(items []dao.DeleteItem) ([]*dao.DeleteResult, error) {
	if len(items) == 0 {
		return nil, uploadError{status: 400, code: "CACHE_INVALID_ARGUMENT", msg: "items is empty"}
	}
	return s.cacheAdminDao.SoftDelete(items)
}

func (s *CacheAdminService) PurgeOrphans(items []dao.DeleteItem) ([]*dao.DeleteResult, error) {
	if len(items) == 0 {
		return nil, uploadError{status: 400, code: "CACHE_INVALID_ARGUMENT", msg: "items is empty"}
	}
	return s.cacheAdminDao.PurgeOrphans(items)
}

// sumDistinctBlobs 按 (repoType, orgRepo, sha) 去重后求和，与 ListRepos/Summary
// 的口径保持一致。
//
// 一行是 (path, sha)：同一个 blob 被仓库里 N 个路径引用就是 N 行，每行都带完整的
// blob 大小。逐行累加等于把去重掉的内容重新算 N 遍——实测一份 1.4GB 的缓存被算成
// 了 20GB。这个数就摆在删除按钮旁边，会被读成"删完能释放多少"，虚高即误导。
//
// 跨仓库不去重：blob 路径含 org/repo，同一份内容在两个仓库下是盘上两个独立文件。
func sumDistinctBlobs(rows []*dao.CacheFileRow) int64 {
	counted := make(map[string]struct{}, len(rows))
	var total int64
	for _, row := range rows {
		key := row.RepoType + "\x00" + row.OrgRepo + "\x00" + row.Sha
		if _, ok := counted[key]; ok {
			continue
		}
		counted[key] = struct{}{}
		total += row.Size
	}
	return total
}

func matchKeyword(keyword string, fields ...string) bool {
	if keyword == "" {
		return true
	}
	needle := strings.ToLower(keyword)
	for _, field := range fields {
		if strings.Contains(strings.ToLower(field), needle) {
			return true
		}
	}
	return false
}

func sortFileRows(rows []*dao.CacheFileRow, key, order string) {
	desc := strings.EqualFold(order, "desc")
	less := func(i, j int) bool { return rows[i].Path < rows[j].Path }
	switch key {
	case "size":
		less = func(i, j int) bool { return rows[i].Size < rows[j].Size }
	case "time":
		less = func(i, j int) bool { return rows[i].BlobMTime < rows[j].BlobMTime }
	case "repo":
		less = func(i, j int) bool { return rows[i].OrgRepo < rows[j].OrgRepo }
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if desc {
			return less(j, i)
		}
		return less(i, j)
	})
}

func sortRecycleRows(rows []*dao.RecycleRow, key, order string) {
	desc := strings.EqualFold(order, "desc")
	less := func(i, j int) bool { return rows[i].UnlinkedAt < rows[j].UnlinkedAt }
	switch key {
	case "size":
		less = func(i, j int) bool { return rows[i].Size < rows[j].Size }
	case "repo":
		less = func(i, j int) bool { return rows[i].OrgRepo < rows[j].OrgRepo }
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if desc {
			return less(j, i)
		}
		return less(i, j)
	})
}

// paginate 把页码钳制到合法范围，越界时返回最后一页而不是空列表：
// 删完最后一页的内容之后前端不刷新页码也能看到东西。
func paginate[T any](rows []T, query CacheQuery) (int, []T) {
	total := len(rows)
	pageSize := query.PageSize
	if pageSize <= 0 {
		pageSize = 50
	}
	if pageSize > 500 {
		pageSize = 500
	}
	page := query.Page
	if page <= 0 {
		page = 1
	}
	maxPage := (total + pageSize - 1) / pageSize
	if maxPage == 0 {
		maxPage = 1
	}
	if page > maxPage {
		page = maxPage
	}
	from := (page - 1) * pageSize
	if from > total {
		from = total
	}
	to := from + pageSize
	if to > total {
		to = total
	}
	return page, rows[from:to]
}
