package handler

import (
	"dingospeed/pkg/repository"
	"github.com/labstack/echo/v4"
	"net/url"
	"strings"
)

func CacheQueryGuard(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if !strings.HasPrefix(c.Request().URL.Path, "/api/cache/") {
			return next(c)
		}
		q, err := url.ParseQuery(c.Request().URL.RawQuery)
		if err != nil {
			return echo.NewHTTPError(400, "invalid query")
		}
		for name, values := range q {
			if name == "orgRepo" || name == "org" {
				return echo.NewHTTPError(400, "use namespace and repo")
			}
			if len(values) != 1 {
				return echo.NewHTTPError(400, "duplicate query: "+name)
			}
		}
		namespace, repo, typ := q.Get("namespace"), q.Get("repo"), q.Get("repoType")
		if typ != "" && typ != "models" && typ != "datasets" && typ != "spaces" {
			return echo.NewHTTPError(400, "invalid repoType")
		}
		if namespace != "" {
			if err := repository.Segment(namespace); err != nil {
				return echo.NewHTTPError(400, err.Error())
			}
		}
		if repo != "" {
			if namespace == "" {
				return echo.NewHTTPError(400, "namespace is required with repo")
			}
			if err := repository.Relative(repo); err != nil {
				return echo.NewHTTPError(400, err.Error())
			}
		}
		return next(c)
	}
}
