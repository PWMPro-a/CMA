package panel

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

func TestServeManagementHTMLDisablesBundleCaching(t *testing.T) {
	service := New("", fstest.MapFS{
		"web/management.html": &fstest.MapFile{Data: []byte("<!doctype html><title>panel</title>")},
	})
	recorder := httptest.NewRecorder()
	service.ServeManagementHTML(recorder, httptest.NewRequest(http.MethodGet, "/management.html", nil), func(w http.ResponseWriter, status int, err error) {
		http.Error(w, err.Error(), status)
	})

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-store, max-age=0" {
		t.Fatalf("Cache-Control = %q", got)
	}
	if got := recorder.Header().Get("Pragma"); got != "no-cache" {
		t.Fatalf("Pragma = %q", got)
	}
}
