// Package api wires HTTP routing, middleware and handlers for the PROSTAFF
// backend. Routing uses Go 1.22+ method+path patterns on http.ServeMux.
package api

import (
	"errors"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"prostaff/internal/atmos"
	"prostaff/internal/config"
	"prostaff/internal/fcm"
	"prostaff/internal/push"
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
	ws *ws.Hub
	// atmos is the payment gateway. Never nil — an unconfigured client reports
	// itself disabled, which is what keeps development working without a
	// merchant account.
	atmos *atmos.Client
	// atmosCallbackNets is ATMOS_CALLBACK_IPS parsed once at start-up rather
	// than per callback. Empty means no address check — see callbackAllowed.
	atmosCallbackNets []*net.IPNet
	// fcm sends push straight to Firebase, which is the only way a notification
	// reaches an Android phone whose app process is gone. Nil when no service
	// account is configured — everything then falls back to Expo, as before.
	fcm     *fcm.Client
	handler http.Handler

	// smsBudget caps what the platform will spend on messages in an hour, and
	// userLimit caps how fast one signed-in account may call the API. Both may
	// be nil, which means unlimited — see ratelimit.go.
	smsBudget *smsBudget
	userLimit *userLimiter

	// batchMu serialises dispatch within one multi-worker request — see
	// lockBatch. Guarded by its own mutex because the map itself is shared.
	batchMuMu sync.Mutex
	batchMu   map[string]*sync.Mutex
}

/**
 * lockBatch serialises the dispatch of orders placed as one request.
 *
 * The five orders behind "I need five helpers" are five POSTs, sent at once, and
 * each dispatches its first wave inline. `batchEngagedWorkers` asks which
 * workers a sibling order is already ringing — and read concurrently, all five
 * asked before any of them had written an answer. Every one of them saw an empty
 * set, and every nearby worker got five cards: exactly the pile this was built
 * to prevent, in the one case it exists for.
 *
 * The waves are therefore taken one at a time per batch. They are short, they
 * are per-request rather than global, and an ordinary single order takes no lock
 * at all.
 */
func (s *Server) lockBatch(batchID string) func() {
	if batchID == "" {
		return func() {}
	}
	s.batchMuMu.Lock()
	mu, ok := s.batchMu[batchID]
	if !ok {
		mu = &sync.Mutex{}
		s.batchMu[batchID] = mu
	}
	s.batchMuMu.Unlock()

	mu.Lock()
	return mu.Unlock
}

// NewServer builds a Server with all routes and middleware wired.
func NewServer(cfg config.Config, st *store.Store, hub *sse.Hub) *Server {
	s := &Server{
		cfg:       cfg,
		store:     st,
		hub:       hub,
		ws:        ws.NewHub(),
		batchMu:   map[string]*sync.Mutex{},
		smsBudget: newSMSBudget(cfg.SMSHourlyBudget, time.Hour),
		userLimit: newUserLimiter(cfg.RateLimitPerMin),
		atmos: atmos.New(atmos.Config{
			BaseURL:        cfg.AtmosBaseURL,
			CheckoutURL:    cfg.AtmosCheckoutURL,
			ConsumerKey:    cfg.AtmosConsumerKey,
			ConsumerSecret: cfg.AtmosConsumerSecret,
			StoreID:        cfg.AtmosStoreID,
			TerminalID:     cfg.AtmosTerminalID,
			APIKey:         cfg.AtmosAPIKey,
			RedirectURL:    cfg.AtmosRedirectURL,
			MPSAPIKey:      cfg.AtmosMPSAPIKey,
			MPSStoreID:     cfg.AtmosMPSStoreID,
		}),
		atmosCallbackNets: parseCIDRs(cfg.AtmosCallbackIPs),
	}
	// A token Expo reports as dead is dropped, so the next order does not queue
	// a push at a device that has not existed since the app was uninstalled.
	push.OnInvalidToken = st.ClearPushToken

	// Firebase, when a service account is configured. Without it the server
	// still works exactly as it did — it just cannot reach a phone whose app is
	// closed, because that is a limitation of Expo's data-only delivery and not
	// something the app can fix. Logged either way: "notifications only arrive
	// while the app is open" is otherwise a mystery from both ends.
	if client, err := fcm.New(cfg.FCMServiceAccountFile, cfg.FCMServiceAccountJSON); err == nil {
		s.fcm = client
		log.Printf("push: firebase enabled for project %s", client.ProjectID())
	} else if !errors.Is(err, fcm.ErrNotConfigured) {
		log.Printf("push: firebase disabled: %v", err)
	} else {
		log.Print("push: firebase not configured — falling back to Expo, " +
			"which cannot deliver to a closed Android app")
	}
	s.handler = recoverMW(logMW(corsMW(cfg.AllowedOrigins)(s.routes())))
	return s
}

// Handler returns the fully wrapped HTTP handler.
func (s *Server) Handler() http.Handler { return s.handler }

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	const p = "/api/v1"

	// Liveness, for the container runtime and whatever watches the domain.
	mux.HandleFunc("GET "+p+"/healthz", s.handleHealth)

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
	mux.HandleFunc("POST "+p+"/notifications/read", s.authed(s.handleReadNotifications))
	// Standing warnings (out of free jobs, subscription about to lapse) — the
	// state of the account rather than a list of things that happened to it.
	mux.HandleFunc("GET "+p+"/alerts", s.authed(s.handleAlerts))

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
	// The worker holding a job says it is done. Cancelling goes through the
	// shared POST /orders/{id}/cancel.
	mux.HandleFunc("POST "+p+"/worker/orders/{id}/complete", s.authed(s.handleWorkerComplete))

	// Company. Becoming a company is an admin action (POST /admin/companies).
	mux.HandleFunc("PATCH "+p+"/company/profile", s.authed(s.handlePatchCompanyProfile))
	mux.HandleFunc("POST "+p+"/company/status", s.authed(s.handleCompanyStatus))
	// A mobile firm (fuel, towing) is also found where its phone is now.
	mux.HandleFunc("POST "+p+"/company/live-location", s.authed(s.handleCompanyLiveLocation))
	mux.HandleFunc("GET "+p+"/company/orders", s.authed(s.handleCompanyOrders))
	mux.HandleFunc("POST "+p+"/company/orders/{id}/accept", s.authed(s.handleCompanyAccept))
	mux.HandleFunc("POST "+p+"/company/orders/{id}/reject", s.authed(s.handleCompanyReject))
	mux.HandleFunc("GET "+p+"/companies", s.authed(s.handleListCompanies))
	mux.HandleFunc("GET "+p+"/companies/{id}", s.authed(s.handleGetCompany))
	mux.HandleFunc("GET "+p+"/companies/{id}/reviews", s.authed(s.handleCompanyReviews))
	mux.HandleFunc("POST "+p+"/companies/{id}/order", s.authed(s.handleCompanyOrder))

	// Orders.
	mux.HandleFunc("POST "+p+"/orders", s.authed(s.handleCreateOrder))
	mux.HandleFunc("GET "+p+"/orders", s.authed(s.handleListOrders))
	mux.HandleFunc("GET "+p+"/orders/{id}", s.authed(s.handleGetOrder))
	mux.HandleFunc("GET "+p+"/orders/{id}/customer", s.authed(s.handleOrderCustomer))
	// Broadcast dispatch: an order is offered to every nearby worker on create;
	// this re-runs the search when nobody took it.
	mux.HandleFunc("POST "+p+"/orders/{id}/dispatch", s.authed(s.handleRedispatch))
	mux.HandleFunc("POST "+p+"/orders/{id}/complete", s.authed(s.handleComplete))
	mux.HandleFunc("POST "+p+"/orders/{id}/cancel", s.authed(s.handleCancel))
	mux.HandleFunc("POST "+p+"/orders/{id}/rate", s.authed(s.handleRate))
	// "Start with the workers I have": closes the places of a multi-worker
	// request still being searched for and releases the found ones to start.
	mux.HandleFunc("POST "+p+"/orders/batch/{batch_id}/start", s.authed(s.handleStartBatch))

	// Wallet and payments. A top-up opens a transaction at Atmos and hands back
	// the invoice URL; nothing is credited until the signed callback lands.
	mux.HandleFunc("GET "+p+"/wallet", s.authed(s.handleGetWallet))
	mux.HandleFunc("POST "+p+"/wallet/topup", s.authed(s.handleTopup))
	mux.HandleFunc("GET "+p+"/payments", s.authed(s.handleListPayments))
	mux.HandleFunc("GET "+p+"/payments/{id}", s.authed(s.handleGetPayment))
	// Paying a payment with a card the app collected, instead of at a page on
	// the gateway. See payments_card.go for why that page is not used.
	mux.HandleFunc("POST "+p+"/payments/{id}/card", s.authed(s.handlePaymentCard))
	mux.HandleFunc("POST "+p+"/payments/{id}/otp", s.authed(s.handlePaymentOTP))
	// The Visa/Mastercard counterpart of /otp: the payer has been through 3DS
	// at their own bank and the transaction can be captured. See
	// payments_card.go for why the two gateways cannot share one endpoint.
	mux.HandleFunc("POST "+p+"/payments/{id}/confirm", s.authed(s.handlePaymentConfirm))
	mux.HandleFunc("POST "+p+"/payments/{id}/resend-otp", s.authed(s.handlePaymentResendOTP))
	// A firm buys its own months off a tariff card. The prices come from the
	// server so the number the firm agrees to is the number it is charged.
	mux.HandleFunc("GET "+p+"/company/subscription/plans", s.authed(s.handleSubscriptionPlans))
	mux.HandleFunc("POST "+p+"/company/subscription/checkout", s.authed(s.handleCompanySubscriptionCheckout))
	// The gateway's callback. Public by necessity — Atmos holds no credentials
	// of ours — and safe because every callback is signature-checked before
	// anything is looked up. This is the URL to register in the merchant
	// cabinet.
	mux.HandleFunc("POST "+p+"/payments/atmos/callback", s.handleAtmosCallback)

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
	// The trial ledger, for undoing a free job spent by mistake.
	mux.HandleFunc("POST "+p+"/admin/users/{id}/free-orders", s.admin(s.handleAdminFreeOrders))
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
	// A firm's account is months, not a balance: this is where they are sold.
	mux.HandleFunc("POST "+p+"/admin/companies/{id}/subscription", s.admin(s.handleAdminExtendSubscription))
	mux.HandleFunc("GET "+p+"/admin/companies/{id}/subscriptions", s.admin(s.handleAdminSubscriptions))
	// Who has bought what, across every firm — the office's sales history.
	mux.HandleFunc("GET "+p+"/admin/subscriptions", s.admin(s.handleAdminAllSubscriptions))
	// Every payment attempt, including the ones that failed and why.
	mux.HandleFunc("GET "+p+"/admin/payments", s.admin(s.handleAdminPayments))
	mux.HandleFunc("GET "+p+"/admin/analytics/orders", s.admin(s.handleAdminAnalytics))
	mux.HandleFunc("GET "+p+"/admin/finance", s.admin(s.handleAdminFinance))
	// The tariff cards a firm chooses between — length and price, nothing else.
	// The service list is the office's to edit — every order and every provider
	// is filed under one of these rows.
	mux.HandleFunc("GET "+p+"/admin/categories", s.admin(s.handleAdminCategories))
	mux.HandleFunc("POST "+p+"/admin/categories", s.admin(s.handleAdminCreateCategory))
	mux.HandleFunc("PATCH "+p+"/admin/categories/{id}", s.admin(s.handleAdminUpdateCategory))
	mux.HandleFunc("DELETE "+p+"/admin/categories/{id}", s.admin(s.handleAdminDeleteCategory))

	mux.HandleFunc("GET "+p+"/admin/subscription-plans", s.admin(s.handleAdminPlans))
	mux.HandleFunc("POST "+p+"/admin/subscription-plans", s.admin(s.handleAdminCreatePlan))
	mux.HandleFunc("PATCH "+p+"/admin/subscription-plans/{id}", s.admin(s.handleAdminUpdatePlan))
	mux.HandleFunc("DELETE "+p+"/admin/subscription-plans/{id}", s.admin(s.handleAdminDeletePlan))
	mux.HandleFunc("GET "+p+"/admin/settings", s.admin(s.handleAdminGetSettings))
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
