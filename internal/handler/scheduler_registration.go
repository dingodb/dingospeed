package handler

import (
	"dingospeed/internal/service"
	"dingospeed/pkg/config"
	"encoding/json"
	"github.com/labstack/echo/v4"
	"io"
	"net/http"
)

// Configuration is intentionally unauthenticated during connectivity rollout.
func SchedulerRegistration(c echo.Context) error {
	if c.Request().Method == http.MethodPut {
		var r config.Registration
		dec := json.NewDecoder(http.MaxBytesReader(c.Response(), c.Request().Body, 4096))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&r); err != nil {
			return c.JSON(400, map[string]string{"error": err.Error()})
		}
		if dec.Decode(&struct{}{}) != io.EOF {
			return c.JSON(400, map[string]string{"error": "expected one JSON object"})
		}
		if err := service.SaveSchedulerRegistration(r); err != nil {
			return c.JSON(409, map[string]string{"error": err.Error()})
		}
	}
	return c.JSON(200, map[string]any{"config": config.SysConfig.Registration(), "status": config.SysConfig.RegistrationState()})
}
