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

package main

import (
	"flag"
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"os"
	"runtime"

	"dingospeed/internal/server"
	"dingospeed/pkg/app"
	"dingospeed/pkg/config"
	log "dingospeed/pkg/logger"
	"dingospeed/pkg/util"
)

var (
	configPath string
	id, _      = os.Hostname() //nolint:errcheck
	Name       = "dingospeed"
	Version    string
)

func init() {
	flag.StringVar(&configPath, "config", "./config/config.yaml", "配置文件路径")
	flag.Parse()
}

func newApp(s *server.HTTPServer, uploadServer *server.UploadServer, uploadCleanupServer *server.UploadCleanupServer, schedulerServer *server.SchedulerServer) *app.App {
	app := app.New(app.ID(id), app.Name(Name), app.Version(Version),
		app.Server(s, uploadServer, uploadCleanupServer, schedulerServer))
	return app
}

func main() {
	conf, err := config.Scan(configPath)
	if err != nil {
		panic(err)
	}

	log.InitLogger()
	// 代理池必须在任何回源请求之前建好，探活循环随之启动。
	if err = util.InitProxyPool(); err != nil {
		panic(err)
	}
	myapp, f, err := wireApp(conf)
	if err != nil {
		panic(err)
	}

	if config.SysConfig.Server.PProf {
		runtime.SetBlockProfileRate(1)
		runtime.SetMutexProfileFraction(1)

		go func() {
			panic(http.ListenAndServe(fmt.Sprintf(":%d", config.SysConfig.Server.PProfPort), nil))
		}()
	}

	defer f()

	err = myapp.Run()
	if err != nil {
		panic(err)
	}
}
