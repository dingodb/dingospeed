package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"

	"dingospeed/internal/router"
	"dingospeed/pkg/config"
	"dingospeed/pkg/middleware"

	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

type UploadServer struct {
	*http.Server
	lis     net.Listener
	network string
	address string
	router  *router.UploadRouter
}

func NewUploadServer(config *config.Config, uploadRouter *router.UploadRouter) *UploadServer {
	// The listen address is whatever upload.host says. It is not forced to
	// loopback here: under bridge networking that made the port unreachable from
	// outside the container, which is why the Spinfield control plane could never
	// talk to it. Leaving an operator's explicit 0.0.0.0 in place is the point.
	//
	// This is not a widening by default: config.Scan still fills an empty
	// upload.host with 127.0.0.1, so every deployment that has not set it keeps
	// binding loopback exactly as before. Opening the port is a configuration
	// decision, and reverting that one config value is the whole rollback.
	host := config.Upload.Host
	s := &UploadServer{
		network: "tcp",
		address: fmt.Sprintf("%s:%d", host, config.Upload.Port),
		router:  uploadRouter,
	}
	s.Server = &http.Server{
		Handler:        uploadRouterEcho(uploadRouter),
		ReadTimeout:    0,
		WriteTimeout:   0,
		MaxHeaderBytes: 1 << 20,
	}
	return s
}

func (s *UploadServer) Start(ctx context.Context) error {
	lis, err := net.Listen(s.network, s.address)
	if err != nil {
		return err
	}
	s.lis = lis
	s.BaseContext = func(net.Listener) context.Context {
		return ctx
	}
	zap.S().Infof("[UPLOAD] server listening on: %s", s.lis.Addr().String())
	if err := s.Serve(s.lis); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *UploadServer) Stop(ctx context.Context) error {
	zap.S().Infof("[UPLOAD] server shutdown.")
	return s.Shutdown(ctx)
}

func NewUploadEngine() router.UploadEcho {
	e := echo.New()
	// 这里刻意不挂 CORSMiddleware，换成 UploadGuardMiddleware。
	//
	// 两者作用不同，不要混淆：CORS 响应头只决定「浏览器允不允许页面读取响应」，
	// 它从不阻止请求到达服务端。摘掉 CORSMiddleware 的收益只有一条——去掉
	// Access-Control-Allow-Origin: *，使恶意页面无法再读取本口的响应
	// （例如 GET /api/cache/repos 枚举缓存清单）。真正挡住写操作的是
	// UploadGuardMiddleware 按 Origin 做的拒绝。
	//
	// 上传口是机器对机器的接口：spinfield 控制面后端调 ingest agent，agent 再调这里，
	// 没有浏览器直连，因此去掉 CORS 头不影响任何现有调用方。
	e.Use(middleware.UploadGuardMiddleware())
	return router.UploadEcho{Echo: e}
}

func uploadRouterEcho(r *router.UploadRouter) *echo.Echo {
	return r.Echo()
}
