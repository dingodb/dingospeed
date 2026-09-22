package middleware

import (
	"github.com/labstack/echo/v4"
	"net/url"
)

// Echo routes against RawPath when it is present. Only in that case are the
// route values still escaped; URL.Path and query values are already decoded.
func DecodeRouteParams(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if c.Request().URL.RawPath != "" {
			values := append([]string(nil), c.ParamValues()...)
			for i, value := range values {
				decoded, err := url.PathUnescape(value)
				if err != nil {
					return echo.NewHTTPError(400, "invalid route encoding")
				}
				values[i] = decoded
			}
			c.SetParamValues(values...)
		}
		return next(c)
	}
}
