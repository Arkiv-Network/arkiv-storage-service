package version

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandlerServesVersion(t *testing.T) {
	oldTag := Tag
	t.Cleanup(func() { Tag = oldTag })
	Tag = "v-test"

	handler := Handler(http.NotFoundHandler())
	req := httptest.NewRequest(http.MethodGet, "/version", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var info Info
	if err := json.NewDecoder(rec.Body).Decode(&info); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if info.Tag != "v-test" {
		t.Fatalf("tag = %q, want %q", info.Tag, "v-test")
	}
}

func TestHandlerDelegatesOtherPaths(t *testing.T) {
	handler := Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusAccepted)
	}
}

func TestHandlerRejectsNonGetVersionRequests(t *testing.T) {
	handler := Handler(http.NotFoundHandler())
	req := httptest.NewRequest(http.MethodPost, "/version", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}
