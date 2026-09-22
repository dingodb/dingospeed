package router

import (
	"github.com/labstack/echo/v4"
	"strings"
)

// Explicit provider endpoints stay unambiguous when an upstream owner has the
// same spelling as a registered hosted namespace.
func providerPrefix(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		req := c.Request()
		for _, provider := range []string{"huggingface", "modelscope"} {
			prefix := "/" + provider
			parts := strings.Split(strings.TrimPrefix(req.URL.Path, "/"), "/")
			file := len(parts) >= 6 && parts[0] == provider && parts[3] == "resolve"
			typedFile := len(parts) >= 7 && parts[0] == provider && (parts[1] == "datasets" || parts[1] == "spaces") && parts[4] == "resolve"
			explicit := provider == "huggingface" && (strings.HasPrefix(req.URL.Path, prefix+"/api/") || file || typedFile) || provider == "modelscope" && strings.HasPrefix(req.URL.Path, prefix+"/api/v1/")
			if explicit {
				c.Set("forcedProvider", provider)
				c.Set("providerPrefix", prefix)
				req.URL.Path = strings.TrimPrefix(req.URL.Path, prefix)
				if req.URL.RawPath != "" {
					req.URL.RawPath = strings.TrimPrefix(req.URL.RawPath, prefix)
				}
				if provider == "modelscope" && !strings.HasPrefix(req.URL.Path, "/api/v1/") {
					return echo.NewHTTPError(404, "unsupported ModelScope protocol route")
				}
				break
			}
		}
		// Hosted HF-compatible downloads use an explicit endpoint. A personal
		// namespace must never intercept the same upstream HF organization.
		parts := strings.Split(strings.TrimPrefix(req.URL.Path, "/"), "/")
		localFile := len(parts) >= 6 && parts[0] == "dingo-local" && parts[3] == "resolve"
		localTypedFile := len(parts) >= 7 && parts[0] == "dingo-local" && (parts[1] == "datasets" || parts[1] == "spaces") && parts[4] == "resolve"
		if strings.HasPrefix(req.URL.Path, "/dingo-local/api/") || localFile || localTypedFile {
			c.Set("forcedLocal", true)
			c.Set("providerPrefix", "/dingo-local")
			req.URL.Path = strings.TrimPrefix(req.URL.Path, "/dingo-local")
			if req.URL.RawPath != "" {
				req.URL.RawPath = strings.TrimPrefix(req.URL.RawPath, "/dingo-local")
			}
		}
		return next(c)
	}
}
