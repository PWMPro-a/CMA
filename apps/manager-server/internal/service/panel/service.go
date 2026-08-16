package panel

import (
	"bytes"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

type Service struct {
	PanelPath       string
	Embedded        fs.FS
	ExpectedVersion string
}

func New(panelPath string, embedded fs.FS, expectedVersion ...string) *Service {
	version := ""
	if len(expectedVersion) > 0 {
		version = strings.TrimSpace(expectedVersion[0])
	}
	return &Service{PanelPath: panelPath, Embedded: embedded, ExpectedVersion: version}
}

func (s *Service) versionMatches(data []byte) bool {
	if s.ExpectedVersion == "" || s.ExpectedVersion == "dev" {
		return true
	}
	return bytes.Contains(data, []byte(s.ExpectedVersion))
}

func (s *Service) serveData(w http.ResponseWriter, r *http.Request, source string, modTime time.Time, data []byte) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-CPAMP-Panel-Source", source)
	w.Header().Set("X-CPAMP-Panel-Version", s.ExpectedVersion)
	http.ServeContent(w, r, "management.html", modTime, bytes.NewReader(data))
}

func (s *Service) ServeManagementHTML(w http.ResponseWriter, r *http.Request, writeError func(http.ResponseWriter, int, error)) {
	// management.html is a single-file bundle with a stable URL. Cache it only
	// once and an older UI can outlive a server rollout, causing it to interpret
	// new API data with stale presentation logic. Always fetch the active build.
	w.Header().Set("Cache-Control", "no-store, max-age=0")
	w.Header().Set("Pragma", "no-cache")
	if s.PanelPath != "" {
		if data, err := os.ReadFile(s.PanelPath); err == nil {
			info, statErr := os.Stat(s.PanelPath)
			if statErr != nil {
				writeError(w, http.StatusInternalServerError, statErr)
				return
			}
			if s.versionMatches(data) {
				s.serveData(w, r, "external", info.ModTime(), data)
				return
			}
		}
	}
	data, err := fs.ReadFile(s.Embedded, "web/management.html")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !s.versionMatches(data) {
		writeError(
			w,
			http.StatusInternalServerError,
			fmt.Errorf("management panel version does not match Manager version %s", s.ExpectedVersion),
		)
		return
	}
	contentType := mime.TypeByExtension(".html")
	if !strings.Contains(contentType, "charset=") {
		contentType += "; charset=utf-8"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("X-CPAMP-Panel-Source", "embedded")
	w.Header().Set("X-CPAMP-Panel-Version", s.ExpectedVersion)
	_, _ = w.Write(data)
}
