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
	"fmt"
	"net/http"

	"dingo-hfmirror/pkg/config"
	"dingo-hfmirror/pkg/consts"
	"dingo-hfmirror/pkg/util"

	"github.com/bytedance/sonic"
	"go.uber.org/zap"
)

type CommitHfSha struct {
	Sha string `json:"sha"`
}

type DownloadDao struct {
}

func NewDownloadDao() *DownloadDao {
	return &DownloadDao{}
}

func (d *DownloadDao) CheckCommitHf(repoType, org, repo, commit, authorization string) bool {
	orgRepo := util.GetOrgRepo(org, repo)
	var url string
	if commit == "" {
		url = fmt.Sprintf("%s/api/%s/%s", config.SysConfig.GetHfEndpoint(), repoType, orgRepo)
	} else {
		url = fmt.Sprintf("%s/api/%s/%s/revision/%s", config.SysConfig.GetHfEndpoint(), repoType, orgRepo, commit)
	}
	headers := map[string]string{}
	if authorization != "" {
		headers["authorization"] = authorization
	}
	resp, err := util.Head(url, headers, consts.ApiTimeOut)
	if err != nil {
		zap.S().Errorf("call %s error.%v", url, err)
		return false
	}
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusTemporaryRedirect {
		return true
	}
	return false
}

func (d *DownloadDao) GetCommitHf(repoType, org, repo, commit, authorization string) string {
	if !config.SysConfig.Online() {
		return d.getCommitHfOffline(repoType, org, repo, commit)
	}
	orgRepo := util.GetOrgRepo(org, repo)
	var url string
	url = fmt.Sprintf("%s/api/%s/%s/revision/%s", config.SysConfig.GetHfEndpoint(), repoType, orgRepo, commit)
	headers := map[string]string{}
	if authorization != "" {
		headers["authorization"] = authorization
	}
	resp, err := util.Get(url, headers, consts.ApiTimeOut)
	if err != nil {
		zap.S().Errorf("call %s error.%v", url, err)
		return d.getCommitHfOffline(repoType, org, repo, commit)
	}
	var sha CommitHfSha
	if err = sonic.Unmarshal(resp, &sha); err != nil {
		zap.S().Errorf("unmarshal error.%v", err)
		return d.getCommitHfOffline(repoType, org, repo, commit)
	}
	return sha.Sha
}

func (d *DownloadDao) getCommitHfOffline(repoType, org, repo, commit string) string {
	return ""
}

func (d *DownloadDao) FileGetGenerator(repoType, org, repo, commit, filePath string, request *http.Request) error {
	orgRepo := util.GetOrgRepo(org, repo)
	headPath := fmt.Sprintf("%s/heads/%s/%s/resolve/%s/%s", config.SysConfig.Repos(), repoType, orgRepo, commit, filePath)
	savePath := fmt.Sprintf("%s/files/%s/%s/resolve/%s/%s", config.SysConfig.Repos(), repoType, orgRepo, commit, filePath)
	err := util.MakeDirs(headPath)
	if err != nil {
		zap.S().Errorf("create %s dir err.%v", headPath, err)
		return err
	}
	err = util.MakeDirs(savePath)
	if err != nil {
		zap.S().Errorf("create %s dir err.%v", savePath, err)
		return err
	}
	var url string
	if repoType == "models" {
		url = fmt.Sprintf("%s/%s/resolve/%s/%s", config.SysConfig.GetHfEndpoint(), orgRepo, commit, filePath)
	} else {
		url = fmt.Sprintf("%s/%s/%s/resolve/%s/%s", config.SysConfig.GetHfEndpoint(), repoType, orgRepo, commit, filePath)
	}
	headers := map[string]string{}
	for k, _ := range request.Header {
		if k == "host" {
			domain, _ := util.GetDomain(url)
			headers[k] = domain
		} else {
			headers[k] = request.Header.Get(k)
		}
	}
	return err
}

func (d *DownloadDao) getPathsInfo() error {
	return nil
}
