package httpapi

import (
	"net/http"

	"github.com/catalyst/billing/web"
)

// WithWeb attaches the embedded frontend.
//
// A Server without it still serves the API; the root path then answers 404 like any
// other unknown path, which is the honest answer for an API-only deployment.
func (s *Server) WithWeb(h *web.Handler) *Server {
	s.web = h
	return s
}

// handleStatic serves the SPA and keeps the API a JSON surface.
//
// The mux routes every unmatched path here (the "/" pattern), so this is the one place
// that decides between "a client-side route, serve the shell" and "an unknown endpoint,
// say so in JSON". Serving HTML for /api/v1/typo is the failure this prevents: a client
// that expected JSON gets a parse error instead of a 404 it can branch on.
func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	if s.web == nil {
		writeProblem(w, http.StatusNotFound, "not_found", "no such endpoint")
		return
	}
	if s.web.IsAPIPath(r.URL.Path) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such endpoint")
		return
	}
	s.web.ServeHTTP(w, r)
}
