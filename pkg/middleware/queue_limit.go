package middleware

import (
	"dingo-hfmirror/pkg/config"
	"dingo-hfmirror/pkg/util"

	"github.com/labstack/echo/v4"
)

var requestQueue chan struct{}

func InitMiddlewareConfig() {
	requestQueue = make(chan struct{}, config.SysConfig.TokenBucketLimit.HandlerCapacity)
}

func QueueLimitMiddleware(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
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
