package middleware

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCompressLargeResponsesCompressesSelectedPanelPayloads(t *testing.T) {
	body := strings.Repeat(`{"name":"account"}`, 1024)
	handler := CompressLargeResponses(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept-Encoding"); got != "" {
			t.Fatalf("forwarded Accept-Encoding = %q, want identity response", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "999999")
		_, _ = io.WriteString(w, body)
	}))
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v0/management/auth-files", nil)
	request.Header.Set("Accept-Encoding", "br, gzip")

	handler.ServeHTTP(recorder, request)

	if got := recorder.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if got := recorder.Header().Get("Content-Length"); got != "" {
		t.Fatalf("Content-Length = %q, want empty", got)
	}
	if got := recorder.Header().Get("Vary"); !strings.Contains(got, "Accept-Encoding") {
		t.Fatalf("Vary = %q, want Accept-Encoding", got)
	}
	reader, err := gzip.NewReader(recorder.Body)
	if err != nil {
		t.Fatalf("new gzip reader: %v", err)
	}
	decoded, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read compressed body: %v", err)
	}
	_ = reader.Close()
	if string(decoded) != body {
		t.Fatalf("decoded body length = %d, want %d", len(decoded), len(body))
	}
}

func TestCompressLargeResponsesLeavesOtherRequestsUnchanged(t *testing.T) {
	handler := CompressLargeResponses(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "plain")
	}))
	for _, path := range []string{"/health", "/v0/management/auth-files/status"} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("Accept-Encoding", "gzip")
		handler.ServeHTTP(recorder, request)
		if got := recorder.Header().Get("Content-Encoding"); got != "" {
			t.Fatalf("%s Content-Encoding = %q, want empty", path, got)
		}
		if got := recorder.Body.String(); got != "plain" {
			t.Fatalf("%s body = %q, want plain", path, got)
		}
	}
}

func TestAcceptsGzipHonorsDisabledEncoding(t *testing.T) {
	if acceptsGzip("gzip;q=0, br") {
		t.Fatal("gzip;q=0 must disable compression")
	}
	if !acceptsGzip("br, gzip;q=1") {
		t.Fatal("gzip;q=1 must enable compression")
	}
}
