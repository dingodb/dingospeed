package service

import (
	"dingo-hfmirror/internal/dao"
	"dingo-hfmirror/pkg/config"
	"dingo-hfmirror/pkg/consts"
	"dingo-hfmirror/pkg/util"

	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

type MetaService struct {
	fileDao *dao.FileDao
	metaDao *dao.MetaDao
}

func NewMetaService(fileDao *dao.FileDao, metaDao *dao.MetaDao) *MetaService {
	return &MetaService{
		fileDao: fileDao,
		metaDao: metaDao,
	}
}

func (d *MetaService) MetaProxyCommon(c echo.Context, repoType, org, repo, commit, method string) error {
	zap.S().Debugf("MetaProxyCommon:%s/%s/%s/%s/%s", repoType, org, repo, commit, method)
	if _, ok := consts.RepoTypesMapping[repoType]; !ok {
		return util.ErrorPageNotFound(c)
	}
	if org == "" && repo == "" {
		return util.ErrorRepoNotFound(c)
	}
	authorization := c.Request().Header.Get("authorization")
	if config.SysConfig.Online() {
		// check repo
		if !d.fileDao.CheckCommitHf(repoType, org, repo, "", authorization) {
			return util.ErrorRepoNotFound(c)
		}
		// check repo commit
		if !d.fileDao.CheckCommitHf(repoType, org, repo, commit, authorization) {
			return util.ErrorRevisionNotFound(c, commit)

		}
	}
	commitSha := d.fileDao.GetCommitHf(repoType, org, repo, commit, authorization)
	if commitSha == "" {
		return util.ErrorRepoNotFound(c)
	}
	var err error
	if config.SysConfig.Online() {
		err = d.metaDao.MetaGetGenerator(c, repoType, org, repo, commitSha, method)
	} else {
		err = d.metaDao.MetaGetGenerator(c, repoType, org, repo, commitSha, method)
	}
	err = d.fileDao.FileGetGenerator(c, repoType, org, repo, commitSha, method, consts.RequestTypeGet)
	return err
}

func (d *MetaService) WhoamiV2(c echo.Context) error {
	err := d.fileDao.WhoamiV2Generator(c)
	return err
}
