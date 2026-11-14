package api

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"prostaff/internal/auth"
	"prostaff/internal/models"
)

type ctxKey int

const userCtxKey ctxKey = iota

// userFrom returns the authenticated user placed in the context by authed.
func userFrom(r *http.Request) *models.User {
	u, _ := r.Context().Value(userCtxKey).(*models.User)
	return u
}

// recoverMW converts panics into a 500 JSON response.
func recoverMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("panic: %v", rec)
				writeError(w, http.StatusInternalServerError, "internal", "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// logMW logs each request with method, path, status and duration.
func logMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &respRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		log.Printf("%s %s %d %s", r.Method, r.URL.Path, rec.status, time.Since(start).Round(time.Millisecond))
	})
}

// corsMW adds CORS headers based on AllowedOrigins config.
func corsMW(allowedOrigins []string) func(http.Handler) http.Handler {
	allowed := make(map[string]bool, len(allowedOrigins))
	for _, o := range allowedOrigins {
		allowed[strings.TrimRight(o, "/")] = true
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			h := w.Header()
			h.Add("Vary", "Origin")

			if len(allowed) == 0 || allowed[strings.TrimRight(origin, "/")] {
				h.Set("Access-Control-Allow-Origin", origin)
			}
			h.Set("Access-Control-Allow-Methods", "GET, POST, PATCH, PUT, DELETE, OPTIONS")
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			h.Set("Access-Control-Max-Age", "86400")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// authed wraps a handler, requiring a valid Bearer JWT and an active user.
func (s *Server) authed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		if token == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing bearer token")
			return
		}
		sub, err := auth.Parse(s.cfg.JWTSecret, token)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "invalid token")
			return
		}
		u, ok := s.store.GetUser(sub)
		if !ok || !u.Active {
			writeError(w, http.StatusUnauthorized, "unauthorized", "unknown or inactive user")
			return
		}
		// Rate limited here rather than at the front door, because here is where
		// the caller has a name. An address would be a carrier NAT with a city
		// behind it; a user id is one account, so the limit can be tight enough
		// to mean something without a shared IP ever being the reason somebody
		// cannot book a cleaner. See ratelimit.go.
		if !s.userLimit.allow(u.ID) {
			writeError(w, http.StatusTooManyRequests, "too_many_requests",
				"too many requests — please slow down")
			return
		}
		ctx := context.WithValue(r.Context(), userCtxKey, u)
		next(w, r.WithContext(ctx))
	}
}

// requireRole wraps a handler, requiring the authenticated user to hold role.
func (s *Server) requireRole(role string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := userFrom(r)
		if u == nil || !u.HasRole(role) {
			writeError(w, http.StatusForbidden, "forbidden", "requires "+role+" role")
			return
		}
		next(w, r)
	}
}

// admin composes auth + admin-role enforcement.
func (s *Server) admin(next http.HandlerFunc) http.HandlerFunc {
	return s.authed(s.requireRole(models.RoleAdmin, next))
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	parts := strings.SplitN(h, " ", 2)
	if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
		return strings.TrimSpace(parts[1])
	}
	return ""
}

// respRecorder captures the status code and forwards Flush for SSE.
type respRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (r *respRecorder) WriteHeader(code int) {
	r.status = code
	r.wrote = true
	r.ResponseWriter.WriteHeader(code)
}

func (r *respRecorder) Write(b []byte) (int, error) {
	if !r.wrote {
		r.wrote = true
	}
	return r.ResponseWriter.Write(b)
}

// Flush implements http.Flusher so the SSE handler can stream through the
// logging wrapper.
func (r *respRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack implements http.Hijacker so a WebSocket can be upgraded through the
// logging wrapper.
//
// Without it the upgrade fails with "response does not implement
// http.Hijacker" — the handler never sees a connection, the client sees only
// "bad handshake", and the log line says 500 with no reason. A wrapper that
// swallows an interface the wrapped writer had is invisible until something
// needs it.
func (r *respRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("api: ResponseWriter does not support hijacking")
	}
	return h.Hijack()
}
