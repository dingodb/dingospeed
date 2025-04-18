package server

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"go.uber.org/zap"
	"html/template"
	"io"
	"net"
	"net/http"

	"dingo-hfmirror/internal/router"
	"dingo-hfmirror/pkg/config"
	"dingo-hfmirror/pkg/middleware"

	"github.com/labstack/echo/v4"
)

type HTTPServer struct {
	*http.Server
	lis     net.Listener
	err     error
	network string
	address string
	http    *router.HttpRouter
}

//go:embed "templates/*.html"
var templatesFS embed.FS

type Template struct {
	templates *template.Template
}

func (t *Template) Render(w io.Writer, name string, data interface{}, c echo.Context) error {
	return t.templates.ExecuteTemplate(w, name, data)
}

func NewServer(config *config.Config, echo *echo.Echo, httpr *router.HttpRouter) *HTTPServer {
	s := &HTTPServer{
		network: "tcp",
		address: fmt.Sprintf("%s:%d", config.Server.Host, config.Server.Port),
		http:    httpr,
	}
	s.Server = &http.Server{
		Handler:        echo,
		ReadTimeout:    0,
		WriteTimeout:   0, // 对用户侧的响应设置永不超时
		MaxHeaderBytes: 1 << 20,
	}
	return s
}

func (s *HTTPServer) Start(ctx context.Context) error {
	lis, err := net.Listen(s.network, s.address)
	if err != nil {
		s.err = err
		return err
	}
	s.lis = lis
	s.BaseContext = func(net.Listener) context.Context {
		return ctx
	}
	zap.S().Infof("[HTTP] server listening on: %s", s.lis.Addr().String())
	if err := s.Serve(s.lis); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *HTTPServer) Stop(ctx context.Context) error {
	zap.S().Infof("[HTTP] server shutdown on: %s", s.lis.Addr().String())
	return s.Shutdown(ctx)
}

func NewEngine() *echo.Echo {
	r := echo.New()
	middleware.InitMiddlewareConfig()
	r.Use(middleware.QueueLimitMiddleware)

	t := &Template{
		templates: template.Must(template.ParseFS(templatesFS, "templates/*.html")),
	}
	r.Renderer = t
	return r
}
