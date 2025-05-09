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

//go:build wireinject
// +build wireinject

// The build tag makes sure the stub is not built in the final build.

package main

import (
	"dingospeed/internal/dao"
	"dingospeed/internal/handler"
	"dingospeed/internal/router"
	"dingospeed/internal/server"
	"dingospeed/internal/service"
	"dingospeed/pkg/app"
	"dingospeed/pkg/config"

	"github.com/google/wire"
)

func wireApp(*config.Config) (*app.App, func(), error) {
	panic(wire.Build(server.ServerProvider, router.RouterProvider, handler.HandlerProvider, service.ServiceProvider, dao.DaoProvider, newApp))
}
