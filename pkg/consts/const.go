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

const HUGGINGFACE_HEADER_X_REPO_COMMIT = "X-Repo-Commit"

const (
	RequestTypeHead = "head"
	RequestTypeGet  = "get"
)

const RespChanSize = 100
const PromSource = "source"
