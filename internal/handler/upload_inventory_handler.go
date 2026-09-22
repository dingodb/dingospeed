package handler

import (
	"crypto/subtle"
	"dingospeed/internal/service"
	"github.com/labstack/echo/v4"
	"net/http"
)

// UploadInventory serves only the durable snapshot already produced by the
// reconciler. Scheduler never interprets a half-finished live filesystem scan.
func UploadInventory(c echo.Context) error {
	snap, err := service.LoadUploadInventorySnapshot()
	if err != nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "upload inventory is not ready")
	}
	provided := c.QueryParam("token")
	if snap.ReportToken == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(snap.ReportToken)) != 1 {
		return echo.NewHTTPError(http.StatusForbidden, "invalid inventory report token")
	}
	response := *snap
	response.ReportToken = ""
	return c.JSON(http.StatusOK, &response)
}
