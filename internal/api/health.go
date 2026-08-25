package api

import "net/http"

// GET /healthz — can this process serve requests?
//
// Public and unauthenticated, because the things that ask are a container
// runtime, a load balancer and an uptime monitor, none of which have an
// account. The Dockerfile used to point its HEALTHCHECK at /categories, which
// needs a token: every container reported itself unhealthy forever, whether or
// not anything was wrong.
//
// It pings the database rather than only answering for itself. A server that is
// listening but cannot reach Postgres answers every real request with a 500,
// and a health check that calls that healthy is worse than none — it is the
// reason nobody restarts the thing that is broken.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Ping(); err != nil {
		writeError(w, http.StatusServiceUnavailable, "unhealthy", "database unreachable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}
