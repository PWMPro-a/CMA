package middleware

import (
	"compress/gzip"
	"net/http"
	"strings"
)

// CompressLargeResponses applies fast gzip compression only to the panel's
// largest non-streaming responses. Keeping the scope explicit avoids changing
// SSE, proxy streaming, and small control-plane responses.
func CompressLargeResponses(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !shouldCompressLargeResponse(r) || !acceptsGzip(r.Header.Get("Accept-Encoding")) {
			next.ServeHTTP(w, r)
			return
		}

		request := r.Clone(r.Context())
		request.Header = r.Header.Clone()
		// The manager owns the single compression layer. This also guarantees
		// proxied responses are not already encoded when they reach the writer.
		request.Header.Del("Accept-Encoding")

		writer, err := gzip.NewWriterLevel(w, gzip.BestSpeed)
		if err != nil {
			next.ServeHTTP(w, request)
			return
		}
		responseWriter := &gzipResponseWriter{ResponseWriter: w, writer: writer}
		next.ServeHTTP(responseWriter, request)
		_ = writer.Close()
	})
}

type gzipResponseWriter struct {
	http.ResponseWriter
	writer      *gzip.Writer
	wroteHeader bool
}

func (w *gzipResponseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.Header().Del("Content-Length")
	w.Header().Set("Content-Encoding", "gzip")
	appendVary(w.Header(), "Accept-Encoding")
	w.ResponseWriter.WriteHeader(status)
}

func (w *gzipResponseWriter) Write(body []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.writer.Write(body)
}

func shouldCompressLargeResponse(r *http.Request) bool {
	if r == nil || r.Method != http.MethodGet {
		return false
	}
	path := strings.TrimRight(r.URL.Path, "/")
	return path == "/management.html" ||
		path == "/v0/management/auth-files" ||
		path == "/v0/management/supply/account-pool"
}

func acceptsGzip(value string) bool {
	for _, item := range strings.Split(value, ",") {
		parts := strings.Split(strings.TrimSpace(item), ";")
		if !strings.EqualFold(strings.TrimSpace(parts[0]), "gzip") {
			continue
		}
		for _, parameter := range parts[1:] {
			if strings.EqualFold(strings.TrimSpace(parameter), "q=0") {
				return false
			}
		}
		return true
	}
	return false
}

func appendVary(header http.Header, value string) {
	for _, current := range header.Values("Vary") {
		for _, item := range strings.Split(current, ",") {
			if strings.EqualFold(strings.TrimSpace(item), value) {
				return
			}
		}
	}
	header.Add("Vary", value)
}
