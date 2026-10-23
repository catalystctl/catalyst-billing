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
//
// It is also where a referral link is cashed in: ?ref= arrives on the document request,
// because that is the URL the affiliate published, and this is the only handler that sees
// the storefront's own navigations. Capturing it here means the code survives until the
// registration POST without the frontend being involved at all.
func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	if s.web == nil {
		writeProblem(w, http.StatusNotFound, "not_found", "no such endpoint")
		return
	}
	if s.web.IsAPIPath(r.URL.Path) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such endpoint")
		return
	}
	s.captureReferralCode(w, r)
	s.web.ServeHTTP(w, r)
}
