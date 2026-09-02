package middleware

import (
	"net"
	"net/http"
	"strings"

	"dingospeed/pkg/config"
	"dingospeed/pkg/consts"
	"dingospeed/pkg/prom"
	"dingospeed/pkg/util"

	"github.com/labstack/echo/v4"
)

var (
	requestQueue      chan struct{}
	fileDownloadQueue chan struct{}
)

func InitMiddlewareConfig() {
	requestQueue = make(chan struct{}, config.SysConfig.TokenBucketLimit.HandlerCapacity)
	fileDownloadQueue = make(chan struct{}, config.SysConfig.TokenBucketLimit.HandlerCapacity*2)
}

func QueueLimitMiddleware(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		url := c.Request().URL.String()
		method := c.Request().Method
		remoteAddr := c.Request().RemoteAddr
		source, _, err := net.SplitHostPort(remoteAddr)
		if err != nil {
			return err
		}
		c.Set(consts.PromSource, source)
		if config.SysConfig.EnableMetric() {
			metrics := strings.Contains(url, "metrics")
			if metrics {
				return next(c)
			}
			if method == echo.GET && strings.Contains(url, "resolve") {
				prom.PromSourceCounter(prom.RequestTotalCnt, source)
				select {
				case fileDownloadQueue <- struct{}{}:
					defer func() {
						<-fileDownloadQueue
					}()
					if err = next(c); err != nil {
						prom.PromSourceCounter(prom.RequestFailCnt, source)
						return err
					} else {
						prom.PromSourceCounter(prom.RequestSuccessCnt, source)
						return nil
					}
				default:
					prom.PromSourceCounter(prom.RequestTooManyCnt, source)
					return util.ErrorTooManyRequest(c)
				}
			} else {
				return nextRequest(c, next)
			}
		} else {
			return nextRequest(c, next)
		}
	}
}

func nextRequest(c echo.Context, next echo.HandlerFunc) error {
	select {
	case requestQueue <- struct{}{}:
		defer func() {
			<-requestQueue
		}()
		return next(c)
	default:
		return util.ErrorTooManyRequest(c)
	}
}

// CORSMiddleware 跨域中间件（适配Echo框架）
func CORSMiddleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			// 设置跨域头
			c.Response().Header().Set("Access-Control-Allow-Origin", "*")
			// 注意 Allow-Methods 只影响预检请求，删掉这里的 POST 并不能阻止跨域 POST：
			// text/plain 的 POST 属于简单请求，压根不发预检。所以这一行不是访问控制，
			// 收紧它只会打断跨域用 JSON 调 /api/cacheJob/* 的调用方（那种请求要预检），
			// 拦不住任何攻击。下载口若要真正限制跨站写入，需要的是按 Origin 判断的
			// 中间件，而不是改这里。
			c.Response().Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS, HEAD")
			c.Response().Header().Set("Access-Control-Allow-Headers", "*")
			c.Response().Header().Set("Access-Control-Expose-Headers", "*")

			// 处理OPTIONS预检请求
			if c.Request().Method == http.MethodOptions {
				return c.NoContent(http.StatusOK)
			}

			return next(c)
		}
	}
}
