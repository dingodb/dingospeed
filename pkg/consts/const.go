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

package consts

import "time"

var RepoTypesMapping = map[string]RepoType{
	"models":   RepoTypeModel,
	"spaces":   RepoTypeSpace,
	"datasets": RepoTypeDataset,
}

// repo类型
type RepoType string

const (
	RepoTypeModel   RepoType = RepoType("models")
	RepoTypeSpace            = RepoType("spaces")
	RepoTypeDataset          = RepoType("datasets")
)

func (a RepoType) Value() string {
	return string(a)
}

const (
	HUGGINGFACE_HEADER_CONTENT_LENGTH = "content-length"
	HUGGINGFACE_HEADER_ETAG           = "etag"
	HUGGINGFACE_HEADER_X_REPO_COMMIT  = "X-Repo-Commit"
	HUGGINGFACE_HEADER_X_LINKED_ETAG  = "X-Linked-Etag"
	HUGGINGFACE_HEADER_X_LINKED_SIZE  = "X-Linked-Size"
	HUGGINGFACE_HEADER_X_XET_HASH     = "X-Xet-Hash"
	HUGGINGFACE_LOCATION              = "Location"
	HUGGINGFACE_Link                  = "Link"
)
const MAX_HTTP_DOWNLOAD_SIZE = 50 * 1000 * 1000 * 1000 // 50 GB

const (
	RequestTypeHead = "head"
	RequestTypeGet  = "get"
)

const RespChanSize = 100
const PromSource = "source"
const PromOrgRepo = "orgRepo"

const (
	VersionOrigin           = 0
	VersionSnapshot         = 1
	SchedulerModeStandalone = "standalone"
	SchedulerModeCluster    = "cluster"
)

var RpcRequestTimeout = time.Duration(300) * time.Second

const (
	Huggingface        = "huggingface"
	Hfmirror           = "hf-mirror"
	RequestSourceInner = "inner"
)

const (
	SchedulerNo  = 1
	SchedulerYes = 2
)

const (
	CacheTypePreheat = 1
	CacheTypeMount   = 2

	RunningStatusJobIng      = 1
	RunningStatusJobBreak    = 2
	RunningStatusJobComplete = 3
	RunningStatusJobStopping = 4
	RunningStatusJobStop     = 5
	RunningStatusJobWait     = 6

	OperationProcess int = 1
	OperationPreheat int = 2
	OperationMount   int = 3
)

const (
	StatusDownloading   = 1
	StatusDownloadBreak = 2
	StatusDownloaded    = 3

	KeyProcessId        = "processId"
	KeyMasterInstanceId = "masterInstanceId"
)

const (
	TaskMoreErrMsg = "当前缓存任务较多导致启动失败，请稍后再启动。"
)
