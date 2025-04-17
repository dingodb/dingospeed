package middleware

import (
	"dingo-hfmirror/pkg/util"

	"github.com/labstack/echo/v4"
	"golang.org/x/time/rate"
)

// 定义一个限流中间件
func RateLimitMiddleware(limiter *rate.Limiter) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if !limiter.Allow() {
				return util.ErrorTooManyRequest(c)
			}
			return next(c)
		}
	}
}
