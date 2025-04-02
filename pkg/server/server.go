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

package server

import (
	"context"
	"embed"
	"fmt"
	"io"
	"net/http"
	"text/template"

	"dingo-hfmirror/internal/router"
	"dingo-hfmirror/pkg/config"

	"github.com/google/wire"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

var ServerProvider = wire.NewSet(NewServer, NewEngine)

type Server struct {
	s    *http.Server
	http *router.HttpRouter
}

//go:embed "templates/*.html"
var templatesFS embed.FS

type Template struct {
	templates *template.Template
}

func (t *Template) Render(w io.Writer, name string, data interface{}, c echo.Context) error {
	return t.templates.ExecuteTemplate(w, name, data)
}

func NewServer(config *config.Config, echo *echo.Echo, httpr *router.HttpRouter) *Server {
	server := &http.Server{
		Addr:           fmt.Sprintf("%s:%d", config.Server.Host, config.Server.Port),
		Handler:        echo,
		ReadTimeout:    0,
		WriteTimeout:   0, // 设置永不超时
		MaxHeaderBytes: 1 << 20,
	}

	s := &Server{
		http: httpr,
		s:    server,
	}

	return s
}

func (s *Server) Start() error {
	zap.S().Infof("server start at %s", s.s.Addr)
	return s.s.ListenAndServe()
}

func (s *Server) Stop(ctx context.Context) error {
	return s.s.Shutdown(ctx)
}

func NewEngine() *echo.Echo {
	r := echo.New()
	// r.Use(gin.Logger())
	// r.Use(gin.Recovery())
	// r.Use(middeware.Cors())
	// r.Use(middeware.Jwt())
	t := &Template{
		templates: template.Must(template.ParseFS(templatesFS, "templates/*.html")),
	}
	r.Renderer = t
	return r
}
