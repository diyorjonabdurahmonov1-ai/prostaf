// Package api wires HTTP routing, middleware and handlers for the PROSTAFF
// backend. Routing uses Go 1.22+ method+path patterns on http.ServeMux.
package api

import (
	"net/http"

	"prostaff/internal/config"
	"prostaff/internal/sse"
	"prostaff/internal/store"
	"prostaff/internal/ws"
)

// Server holds shared dependencies and the built HTTP handler.
type Server struct {
	cfg   config.Config
	store *store.Store
	hub   *sse.Hub
	// ws is the realtime transport the mobile app actually uses; `hub` (SSE)
	// stays for the web build. Handlers publish through s.publish, not either.
	ws      *ws.Hub
	handler http.Handler
}

// NewServer builds a Server with all routes and middleware wired.
func NewServer(cfg config.Config, st *store.Store, hub *sse.Hub) *Server {
	s := &Server{cfg: cfg, store: st, hub: hub, ws: ws.NewHub()}
	s.handler = recoverMW(logMW(corsMW(cfg.AllowedOrigins)(s.routes())))
	return s
}

// Handler returns the fully wrapped HTTP handler.
func (s *Server) Handler() http.Handler { return s.handler }

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	const p = "/api/v1"

	// Auth (public). There is no /auth/register: an account is only ever opened
	// through /auth/otp/verify, so every account has a proven phone behind it.
	mux.HandleFunc("POST "+p+"/auth/login", s.handleLogin)
	// Password reset is three steps: send a code, spend it for a reset token,
	// then choose the new password.
	mux.HandleFunc("POST "+p+"/auth/forgot-password", s.handleForgotPassword)
	mux.HandleFunc("POST "+p+"/auth/forgot-password/verify", s.handleForgotPasswordVerify)
	mux.HandleFunc("POST "+p+"/auth/reset-password", s.handleResetPassword)
	mux.HandleFunc("POST "+p+"/auth/otp/request", s.handleOTPRequest)
	mux.HandleFunc("POST "+p+"/auth/otp/verify", s.handleOTPVerify)
	mux.HandleFunc("POST "+p+"/auth/admin/login", s.handleAdminLogin)
	mux.HandleFunc("POST "+p+"/auth/refresh", s.handleRefresh)
	mux.HandleFunc("POST "+p+"/auth/logout", s.handleLogout)

	// Realtime. The WebSocket is the primary transport — React Native has no
	// EventSource, so SSE never reached the phones at all. Both auth via ?token=
	// because a handshake carries no headers we control.
	mux.HandleFunc("GET "+p+"/ws", s.handleWS)
	mux.HandleFunc("GET "+p+"/events", s.handleEvents)

	// Me / reference.
	mux.HandleFunc("GET "+p+"/me", s.authed(s.handleMe))
	mux.HandleFunc("POST "+p+"/me/push-token", s.authed(s.handleSetPushToken))
	mux.HandleFunc("POST "+p+"/me/avatar", s.authed(s.handleSetAvatar))
	// Sends a real Expo push to the caller's own device and reports the ticket,
	// so "push is broken" can be diagnosed from the phone in one tap.
	mux.HandleFunc("POST "+p+"/me/push-test", s.authed(s.handlePushTest))
	mux.HandleFunc("GET "+p+"/categories", s.authed(s.handleCategories))
	mux.HandleFunc("GET "+p+"/notifications", s.authed(s.handleNotifications))

	// Images. Uploading needs an account; reading does not, because an <Image>
	// tag sends no Authorization header and the id is an unguessable uuid.
	mux.HandleFunc("POST "+p+"/uploads", s.authed(s.handleUpload))
	mux.HandleFunc("GET "+p+"/files/{id}", s.handleGetFile)

	// Worker. Becoming a worker is an admin action (POST /admin/workers) — the
	// app can only edit price/categories and go online.
	mux.HandleFunc("PATCH "+p+"/worker/profile", s.authed(s.handlePatchWorkerProfile))
	mux.HandleFunc("POST "+p+"/worker/status", s.authed(s.handleWorkerStatus))
	mux.HandleFunc("GET "+p+"/worker/orders", s.authed(s.handleWorkerOrders))
	mux.HandleFunc("GET "+p+"/workers/nearby", s.authed(s.handleNearbyWorkers))
	mux.HandleFunc("GET "+p+"/workers/{id}", s.authed(s.handleGetWorker))
	mux.HandleFunc("GET "+p+"/workers/{id}/reviews", s.authed(s.handleWorkerReviews))
	mux.HandleFunc("POST "+p+"/worker/orders/{id}/accept", s.authed(s.handleWorkerAccept))
	mux.HandleFunc("POST "+p+"/worker/orders/{id}/reject", s.authed(s.handleWorkerReject))

	// Company. Becoming a company is an admin action (POST /admin/companies).
	mux.HandleFunc("PATCH "+p+"/company/profile", s.authed(s.handlePatchCompanyProfile))
	mux.HandleFunc("POST "+p+"/company/status", s.authed(s.handleCompanyStatus))
	mux.HandleFunc("GET "+p+"/company/orders", s.authed(s.handleCompanyOrders))
	mux.HandleFunc("POST "+p+"/company/orders/{id}/accept", s.authed(s.handleCompanyAccept))
	mux.HandleFunc("POST "+p+"/company/orders/{id}/reject", s.authed(s.handleCompanyReject))
	mux.HandleFunc("GET "+p+"/companies", s.authed(s.handleListCompanies))
	mux.HandleFunc("GET "+p+"/companies/{id}", s.authed(s.handleGetCompany))
	mux.HandleFunc("POST "+p+"/companies/{id}/order", s.authed(s.handleCompanyOrder))

	// Orders.
	mux.HandleFunc("POST "+p+"/orders", s.authed(s.handleCreateOrder))
	mux.HandleFunc("GET "+p+"/orders", s.authed(s.handleListOrders))
	mux.HandleFunc("GET "+p+"/orders/{id}", s.authed(s.handleGetOrder))
	// Broadcast dispatch: an order is offered to every nearby worker on create;
	// this re-runs the search when nobody took it.
	mux.HandleFunc("POST "+p+"/orders/{id}/dispatch", s.authed(s.handleRedispatch))
	mux.HandleFunc("POST "+p+"/orders/{id}/complete", s.authed(s.handleComplete))
	mux.HandleFunc("POST "+p+"/orders/{id}/cancel", s.authed(s.handleCancel))
	mux.HandleFunc("POST "+p+"/orders/{id}/rate", s.authed(s.handleRate))

	// Wallet.
	mux.HandleFunc("GET "+p+"/wallet", s.authed(s.handleGetWallet))
	mux.HandleFunc("POST "+p+"/wallet/topup", s.authed(s.handleTopup))

	// Admin.
	mux.HandleFunc("GET "+p+"/admin/stats", s.admin(s.handleAdminStats))
	// Who is holding a socket right now — the missing half of "why did the
	// order not reach the worker?".
	mux.HandleFunc("GET "+p+"/admin/realtime", s.admin(s.handleAdminRealtime))
	mux.HandleFunc("GET "+p+"/admin/orders", s.admin(s.handleAdminOrders))
	mux.HandleFunc("GET "+p+"/admin/users", s.admin(s.handleAdminUsers))
	mux.HandleFunc("GET "+p+"/admin/users/{id}", s.admin(s.handleAdminGetUser))
	mux.HandleFunc("PATCH "+p+"/admin/users/{id}", s.admin(s.handleAdminUpdateUser))
	mux.HandleFunc("POST "+p+"/admin/users/{id}/roles", s.admin(s.handleAdminRoles))
	mux.HandleFunc("POST "+p+"/admin/users/{id}/push", s.admin(s.handleAdminPush))
	mux.HandleFunc("POST "+p+"/admin/users/{id}/balance", s.admin(s.handleAdminBalance))
	mux.HandleFunc("POST "+p+"/admin/users/{id}/credit-limit", s.admin(s.handleAdminUserCreditLimit))
	// Provider onboarding — the only way to become a worker or a company.
	mux.HandleFunc("POST "+p+"/admin/workers", s.admin(s.handleAdminCreateWorker))
	mux.HandleFunc("PATCH "+p+"/admin/workers/{id}", s.admin(s.handleAdminUpdateWorker))
	mux.HandleFunc("POST "+p+"/admin/companies", s.admin(s.handleAdminCreateCompany))
	mux.HandleFunc("POST "+p+"/admin/workers/{id}/ban", s.admin(s.handleAdminBanWorker))
	mux.HandleFunc("POST "+p+"/admin/workers/{id}/unban", s.admin(s.handleAdminUnbanWorker))
	mux.HandleFunc("POST "+p+"/admin/workers/{id}/status", s.admin(s.handleAdminWorkerStatus))
	mux.HandleFunc("POST "+p+"/admin/workers/{id}/credit-limit", s.admin(s.handleAdminCreditLimit))
	mux.HandleFunc("GET "+p+"/admin/companies", s.admin(s.handleAdminCompanies))
	mux.HandleFunc("POST "+p+"/admin/companies/{id}/ban", s.admin(s.handleAdminBanCompany))
	mux.HandleFunc("POST "+p+"/admin/companies/{id}/unban", s.admin(s.handleAdminUnbanCompany))
	mux.HandleFunc("POST "+p+"/admin/companies/{id}/status", s.admin(s.handleAdminCompanyStatus))
	mux.HandleFunc("PATCH "+p+"/admin/companies/{id}", s.admin(s.handleAdminUpdateCompany))
	mux.HandleFunc("GET "+p+"/admin/analytics/orders", s.admin(s.handleAdminAnalytics))
	mux.HandleFunc("GET "+p+"/admin/finance", s.admin(s.handleAdminFinance))
	mux.HandleFunc("PATCH "+p+"/admin/settings", s.admin(s.handleAdminSettings))

	// Health.
	mux.HandleFunc("GET "+p+"/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})

	// Swagger spec.
	mux.HandleFunc("GET "+p+"/swagger.yaml", s.handleSwaggerSpec)
	mux.HandleFunc("GET /swagger", s.handleSwaggerUI)
	mux.HandleFunc("GET /swagger/", s.handleSwaggerUI)

	return mux
}
