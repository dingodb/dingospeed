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

package router

import (
	"dingo-hfmirror/internal/handler"

	"github.com/labstack/echo/v4"
)

type HttpRouter struct {
	echo            *echo.Echo
	downloadHandler *handler.DownloadHandler
}

func NewHttpRouter(echo *echo.Echo, downloadHandler *handler.DownloadHandler) *HttpRouter {
	r := &HttpRouter{
		echo:            echo,
		downloadHandler: downloadHandler,
	}
	r.initFlowRouter()
	return r
}

func (r *HttpRouter) initFlowRouter() {
	r.echo.HEAD("/:repoType/:org/:repo/resolve/:commit/:filePath", r.downloadHandler.HeadDownloadHandler1)
	r.echo.HEAD("/:orgOrRepoType/:repo/resolve/:commit/:filePath", r.downloadHandler.HeadDownloadHandler2)
	r.echo.HEAD("/:repo/resolve/:commit/:filePath", r.downloadHandler.HeadDownloadHandler3)

	r.echo.GET("/:org_or_repo_type/:repo_name/resolve/:commit/:file_path", r.downloadHandler.GetDownloadHandler)

}
