package middleware

import (
	"net"
	"strings"

	"dingospeed/pkg/config"
	"dingospeed/pkg/consts"
	"dingospeed/pkg/prom"
	"dingospeed/pkg/util"

	"github.com/labstack/echo/v4"
)

var requestQueue chan struct{}

func InitMiddlewareConfig() {
	requestQueue = make(chan struct{}, config.SysConfig.TokenBucketLimit.HandlerCapacity)
}

func QueueLimitMiddleware(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		url := c.Request().URL.String()
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
			promFlag := strings.Contains(url, "resolve") || strings.Contains(url, "revision")
			if promFlag {
				prom.PromSourceCounter(prom.RequestTotalCnt, source)
				select {
				case requestQueue <- struct{}{}:
					defer func() {
						<-requestQueue
					}()
					if err := next(c); err != nil {
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
