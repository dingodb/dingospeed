package handler

import (
	"dingospeed/pkg/transfersettings"
	"encoding/json"
	"github.com/labstack/echo/v4"
	"io"
	"net/http"
	"strings"
)

func TransferSettings(c echo.Context) error {
	if c.Request().Method == http.MethodPut {
		var s transfersettings.Settings
		dec := json.NewDecoder(io.LimitReader(c.Request().Body, 4096))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&s); err != nil {
			return c.JSON(400, map[string]string{"error": err.Error()})
		}
		var extra any
		if err := dec.Decode(&extra); err != io.EOF {
			return c.JSON(400, map[string]string{"error": "expected one JSON object"})
		}
		if err := transfersettings.Validate(s); err != nil {
			return c.JSON(400, map[string]string{"error": err.Error()})
		}
		if err := transfersettings.Save(s); err != nil {
			return c.JSON(500, map[string]string{"error": err.Error()})
		}
	}
	return c.JSON(200, transfersettings.Current())
}

func LimitUpload(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		release, err := transfersettings.AcquireUpload(c.Request().Context())
		if err != nil {
			return err
		}
		defer release()
		return next(c)
	}
}

func LimitDownload(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		path := c.Path()
		data := strings.Contains(path, "/resolve/") || strings.HasSuffix(path, "/local-file") || strings.HasSuffix(path, "/file") || strings.HasSuffix(path, "/archive") || path == "/api/v1/:repoType/:org/:repo/repo"
		if !data {
			return next(c)
		}
		release, err := transfersettings.AcquireDownload(c.Request().Context())
		if err != nil {
			return err
		}
		defer release()
		return next(c)
	}
}
