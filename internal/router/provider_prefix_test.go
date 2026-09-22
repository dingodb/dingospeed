package router

import (
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
)

func TestProviderPrefixPreservesOriginalOwnerNames(t *testing.T) {
	for _, tc := range []struct {
		input, path string
		local       bool
	}{
		{"/huggingface/demo/resolve/main/weights.bin", "/huggingface/demo/resolve/main/weights.bin", false},
		{"/modelscope/demo/resolve/main/nested/weights.bin", "/modelscope/demo/resolve/main/nested/weights.bin", false},
		{"/huggingface/team/demo/resolve/main/weights.bin", "/team/demo/resolve/main/weights.bin", false},
		{"/dingo-local/demo/resolve/main/nested/weights.bin", "/dingo-local/demo/resolve/main/nested/weights.bin", false},
		{"/dingo-local/alice/demo/resolve/main/weights.bin", "/alice/demo/resolve/main/weights.bin", true},
		{"/dingo-local/datasets/alice/demo/resolve/main/weights.bin", "/datasets/alice/demo/resolve/main/weights.bin", true},
		{"/dingo-local/api/models/alice/demo/revision/main", "/api/models/alice/demo/revision/main", true},
	} {
		c := echo.New().NewContext(httptest.NewRequest("GET", tc.input, nil), httptest.NewRecorder())
		err := providerPrefix(func(c echo.Context) error {
			if c.Request().URL.Path != tc.path || (c.Get("forcedLocal") == true) != tc.local {
				t.Fatalf("%s resolved to %s local=%v", tc.input, c.Request().URL.Path, c.Get("forcedLocal"))
			}
			return nil
		})(c)
		if err != nil {
			t.Fatal(err)
		}
	}
}
