package server

import (
	"bytes"
	"encoding/json"
	"net/http"

	"github.com/Gandalf-Le-Dev/pilot/internal/statuspage"
)

// PublicHandler serves the page. Every route is GET; anything else is refused
// by the mux, so nothing reachable from the internet can change state.
func (s *Server) PublicHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handlePage)
	mux.HandleFunc("GET /status.json", s.handleJSON)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	return hardened(mux)
}

func (s *Server) handlePage(w http.ResponseWriter, _ *http.Request) {
	var buf bytes.Buffer
	if err := statuspage.Render(&buf, s.Page()); err != nil {
		logf("rendering the page: %v", err)
		http.Error(w, "unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", statuspage.ContentSecurityPolicy)
	_, _ = w.Write(buf.Bytes())
}

func (s *Server) handleJSON(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.Page())
}

// hardened sets the headers every public response carries. Freshness is the
// page's whole value, so nothing is cached.
func hardened(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}
