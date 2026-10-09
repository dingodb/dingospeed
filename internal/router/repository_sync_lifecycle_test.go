package router

import (
	"net/http/httptest"
	"testing"
)

func TestRepositorySyncStandaloneHistoryRemainsReadable(t *testing.T) {
	_, upload := namespaceEngines(t)
	for _, path := range []string{"/api/repository-sync/tasks", "/api/repository-sync/tasks/missing"} {
		w := httptest.NewRecorder()
		upload.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 && w.Code != 404 {
			t.Fatalf("local history blocked: %s %d %s", path, w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	upload.ServeHTTP(w, httptest.NewRequest("POST", "/api/repository-sync/tasks/missing/resume", nil))
	if w.Code != 409 {
		t.Fatalf("standalone resume admitted: %d %s", w.Code, w.Body.String())
	}
}
