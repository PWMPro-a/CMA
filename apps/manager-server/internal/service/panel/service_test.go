package panel

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func TestServeManagementHTMLRejectsMismatchedExternalAndEmbeddedPanel(t *testing.T) {
	panelPath := filepath.Join(t.TempDir(), "management.html")
	if err := os.WriteFile(panelPath, []byte("<title>old-panel</title>"), 0o600); err != nil {
		t.Fatalf("write external panel: %v", err)
	}
	service := New(panelPath, fstest.MapFS{
		"web/management.html": &fstest.MapFile{Data: []byte("<title>old-embedded</title>")},
	}, "20260816-152233")
	recorder := httptest.NewRecorder()
	service.ServeManagementHTML(recorder, httptest.NewRequest(http.MethodGet, "/management.html", nil), func(w http.ResponseWriter, status int, err error) {
		http.Error(w, err.Error(), status)
	})

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusInternalServerError)
	}
}

func TestServeManagementHTMLFallsBackToMatchingEmbeddedPanel(t *testing.T) {
	panelPath := filepath.Join(t.TempDir(), "management.html")
	if err := os.WriteFile(panelPath, []byte("<title>old-panel</title>"), 0o600); err != nil {
		t.Fatalf("write external panel: %v", err)
	}
	service := New(panelPath, fstest.MapFS{
		"web/management.html": &fstest.MapFile{Data: []byte("<title>20260816-152233</title>")},
	}, "20260816-152233")
	recorder := httptest.NewRecorder()
	service.ServeManagementHTML(recorder, httptest.NewRequest(http.MethodGet, "/management.html", nil), func(w http.ResponseWriter, status int, err error) {
		http.Error(w, err.Error(), status)
	})

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if got := recorder.Header().Get("X-CPAMP-Panel-Source"); got != "embedded" {
		t.Fatalf("X-CPAMP-Panel-Source = %q, want embedded", got)
	}
}
