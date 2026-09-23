package util

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"syscall"
	"time"

	"dingospeed/pkg/config"
	myerr "dingospeed/pkg/error"
	"github.com/avast/retry-go"
)

// RetryDownloadContext applies only to idempotent ranged reads. The caller must
// advance its Range after partial delivery before the next attempt.
func RetryDownloadContext(ctx context.Context, read func() error) error {
	attempts := max(config.SysConfig.Retry.Attempts, 1)
	return retry.Do(func() error {
		if err := ctx.Err(); err != nil {
			return retry.Unrecoverable(err)
		}
		err := read()
		if ctx.Err() != nil {
			return retry.Unrecoverable(ctx.Err())
		}
		if err != nil && !retryableDownloadError(err) {
			return retry.Unrecoverable(err)
		}
		return err
	}, retry.Attempts(attempts),
		retry.Delay(time.Duration(config.SysConfig.Retry.Delay)*time.Second),
		retry.DelayType(retry.BackOffDelay), retry.MaxDelay(30*time.Second),
		retry.Context(ctx), retry.LastErrorOnly(true))
}

func retryableDownloadError(err error) bool {
	// A child HTTP client's timeout is retryable while the task context lives.
	// Explicit cancellation and local filesystem failures are never retried.
	if errors.Is(err, context.Canceled) {
		return false
	}
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return false
	}
	var status myerr.Error
	if errors.As(err, &status) {
		switch status.StatusCode() {
		case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusInternalServerError,
			http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true
		default:
			return false
		}
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EPIPE) {
		return true
	}
	var network net.Error
	return errors.As(err, &network) && (network.Timeout() || network.Temporary())
}

// CacheFailureReason is safe for an HTTP trailer and for display in a task.
// Raw errors can contain provider URLs, credentials or local storage paths.
func CacheFailureReason(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "download timed out (context deadline exceeded)"
	case errors.Is(err, context.Canceled):
		return "download canceled"
	case errors.Is(err, io.ErrUnexpectedEOF):
		return "upstream response ended before the file was complete"
	case errors.Is(err, syscall.ENOSPC):
		return "insufficient cache disk space"
	case errors.Is(err, os.ErrPermission):
		return "cache storage permission denied"
	}
	var status myerr.Error
	if errors.As(err, &status) && status.StatusCode() >= 400 {
		return "upstream returned HTTP " + Itoa(status.StatusCode())
	}
	var network net.Error
	if errors.As(err, &network) {
		if network.Timeout() {
			return "download timed out"
		}
		return "upstream network request failed"
	}
	return "cache write or transfer failed; see node logs for details"
}
