package middleware

import (
	"strings"

	"dingo-hfmirror/pkg/config"
	"dingo-hfmirror/pkg/prom"
	"dingo-hfmirror/pkg/util"

	"github.com/labstack/echo/v4"
)

var requestQueue chan struct{}

func InitMiddlewareConfig() {
	requestQueue = make(chan struct{}, config.SysConfig.TokenBucketLimit.HandlerCapacity)
}

func QueueLimitMiddleware(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		url := c.Request().URL.String()
		promFlag := strings.Contains(url, "resolve") || strings.Contains(url, "revision")
		if promFlag {
			prom.RequestTotalCnt.Inc()
			select {
			case requestQueue <- struct{}{}:
				defer func() {
					prom.RequestDoneCnt.Inc()
					<-requestQueue
				}()
				return next(c)
			default:
				prom.RequestTooManyCnt.Inc()
				return util.ErrorTooManyRequest(c)
			}
		} else {
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
	}
}
