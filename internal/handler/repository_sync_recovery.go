package handler

import (
	"context"
	"errors"
	"os"

	"github.com/labstack/echo/v4"
)

func (h *RepositorySyncHandler) RunRecovery(ctx context.Context) { h.service.RunRecovery(ctx) }

func (h *RepositorySyncHandler) Reconcile(c echo.Context) error {
	status, err := h.service.Reconcile(c.Param("taskId"))
	if errors.Is(err, os.ErrNotExist) {
		return c.NoContent(404)
	}
	if err != nil {
		return writeUploadError(c, "sync result inspection failed", err)
	}
	return c.JSON(200, status)
}
