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
	commitSha, err := d.fileDao.GetCommitHf(repoType, org, repo, commit, authorization)
	if err != nil {
		zap.S().Errorf("GetCommitHf err.%v", err)
		return util.ErrorRepoNotFound(c)
	}
	return d.metaDao.MetaGetGenerator(c, repoType, org, repo, commitSha, method)
}

func (d *MetaService) WhoamiV2(c echo.Context) error {
	err := d.fileDao.WhoamiV2Generator(c)
	return err
}

func (d *MetaService) Repos(c echo.Context) error {
	err := d.fileDao.ReposGenerator(c)
	return err
}
