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
}

func NewMetaService(fileDao *dao.FileDao) *MetaService {
	return &MetaService{
		fileDao: fileDao,
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
		if !d.fileDao.CheckCommitHf(repoType, org, repo, commit, authorization) {
			return util.ErrorRepoNotFound(c)
		}
	}
	commitSha := d.fileDao.GetCommitHf(repoType, org, repo, commit, authorization)
	if commitSha == "" {
		return util.ErrorRepoNotFound(c)
	}
	err := d.fileDao.FileGetGenerator(c, repoType, org, repo, commitSha, method, consts.RequestTypeGet)
	return err
}

func (d *MetaService) WhoamiV2(c echo.Context) error {
	err := d.fileDao.WhoamiV2Generator(c)
	return err
}
