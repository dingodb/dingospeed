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
	"context"
	"errors"
	"flag"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"dingo-hfmirror/pkg/config"
	log "dingo-hfmirror/pkg/logger"
	"dingo-hfmirror/pkg/server"

	"golang.org/x/sync/errgroup"
)

var (
	mode       string
	configPath string
	Version    string
)

func init() {
	flag.StringVar(&mode, "mode", "", "运行模式 debug/release")
	flag.StringVar(&configPath, "config", "./config/config.yaml", "配置文件路径")
	flag.Parse()
}

type App struct {
	s *server.Server
}

func (a *App) Stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return a.s.Stop(ctx)
}

func (a *App) Run() error {
	eg, ctx := errgroup.WithContext(context.Background())
	eg.Go(a.s.Start)

	c := make(chan os.Signal, 1)
	// go 不允许监听 kill stop 信号
	signal.Notify(c, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGINT)
	eg.Go(func() error {
		select {
		case <-ctx.Done():
			return nil
		case <-c:
			return a.Stop()
		}
	})

	if err := eg.Wait(); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func newApp(s *server.Server) *App {
	return &App{s: s}
}

func main() {
	conf, err := config.Scan(configPath)
	if err != nil {
		panic(err)
	}

	log.InitLogger()
	app, f, err := wireApp(conf)
	if err != nil {
		panic(err)
	}

	if config.SysConfig.Server.PProf {
		runtime.SetBlockProfileRate(1)
		runtime.SetMutexProfileFraction(1)

		go func() {
			// pprof性能分析 https://blog.csdn.net/water1209/article/details/126778930
			panic(http.ListenAndServe(":6060", nil))
		}()
	}

	defer f()

	err = app.Run()
	if err != nil {
		panic(err)
	}
}
