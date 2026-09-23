package handler

import (
	"dingospeed/pkg/config"
	"dingospeed/pkg/inventory"
	"encoding/json"
	"github.com/labstack/echo/v4"
	"io"
	"net/http"
)

func UploadReconcile(c echo.Context) error {
	var p struct {
		Epoch string `json:"epoch"`
	}
	d := json.NewDecoder(http.MaxBytesReader(c.Response(), c.Request().Body, 4096))
	d.DisallowUnknownFields()
	if err := d.Decode(&p); err != nil {
		return echo.NewHTTPError(400, err.Error())
	}
	if d.Decode(&struct{}{}) != io.EOF || len(p.Epoch) != 36 {
		return echo.NewHTTPError(400, "invalid reconciliation epoch")
	}
	if err := inventory.RequestReconcile(config.SysConfig.Repos(), p.Epoch); err != nil {
		return echo.NewHTTPError(409, err.Error())
	}
	return c.JSON(202, map[string]string{"status": "pending", "epoch": p.Epoch})
}
