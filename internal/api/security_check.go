package api

import (
	"log"
	"net/http"
)

// A finding of the self-check: how bad it is, and what to do — worded for the
// person who runs the platform, not for a developer.
type SecurityCheck struct {
	Code   string `json:"code"`
	Level  string `json:"level"` // "ok", "info", "warning" or "critical"
	Title  string `json:"title"`
	Detail string `json:"detail,omitempty"`
}

const (
	defaultJWTSecret     = "prostaff-dev-secret-change-me"
	defaultAdminPassword = "admin123"
)

// SecurityChecks asks the running server about the settings that decide whether
// the platform can be taken over, and answers for the configuration it is
// actually running with — which nobody reading the repository can see.
func (s *Server) SecurityChecks() []SecurityCheck {
	var out []SecurityCheck
	add := func(code, level, title, detail string) {
		out = append(out, SecurityCheck{Code: code, Level: level, Title: title, Detail: detail})
	}

	switch {
	case s.cfg.JWTSecret == defaultJWTSecret:
		add("jwt_secret", "critical", "Kirish kaliti (JWT_SECRET) standart",
			"Bu kalit ochiq kodda yozilgan, uni bilgan har kim istalgan foydalanuvchi, jumladan admin nomidan kira oladi. Serverdagi .env faylida uzun tasodifiy qiymat yozing.")
	case len(s.cfg.JWTSecret) < 32:
		add("jwt_secret", "warning", "Kirish kaliti (JWT_SECRET) qisqa",
			"Kamida 32 belgili tasodifiy qiymat ishlating.")
	default:
		add("jwt_secret", "ok", "Kirish kaliti o'rnatilgan", "")
	}

	adminHashed := false
	if u, ok := s.store.GetUserByEmail(s.cfg.AdminEmail); ok {
		adminHashed = s.store.GetPasswordHash(u.ID) != ""
	}
	switch {
	case adminHashed:
		add("admin_password", "ok", "Admin paroli o'zgartirilgan", "")
	case s.cfg.AdminPassword == defaultAdminPassword:
		add("admin_password", "critical", "Admin paroli standart (admin123)",
			"Bu parol ochiq kodda yozilgan. Serverdagi .env faylida ADMIN_PASSWORD ni almashtiring.")
	case len(s.cfg.AdminPassword) < 10:
		add("admin_password", "warning", "Admin paroli qisqa", "Kamida 10 belgili parol qo'ying.")
	default:
		add("admin_password", "ok", "Admin paroli o'rnatilgan", "")
	}

	if s.cfg.DevMode {
		add("dev_mode", "warning", "DEV_MODE yoqilgan",
			"Sinov rejimi yoqilgan: u faqat serverning o'zidan kelgan so'rovlarda ishlaydi, internetdan kelganlarda o'chiq. Baribir .env da DEV_MODE=false qiling.")
	} else {
		add("dev_mode", "ok", "Sinov rejimi o'chiq", "")
	}

	if s.atmos.Enabled() {
		if len(s.atmosCallbackNets) == 0 {
			add("atmos_callback", "warning", "To'lov tizimi manzillari cheklanmagan",
				"To'lov xabarlari imzo bilan tekshiriladi, lekin ATMOS_CALLBACK_IPS ni ham yozsangiz, faqat Atmos manzillaridan qabul qilinadi.")
		} else {
			add("atmos_callback", "ok", "To'lov xabarlari faqat Atmos manzillaridan qabul qilinadi", "")
		}
	}

	if s.fcm == nil {
		add("push", "info", "Firebase ulanmagan",
			"Ilova yopiq bo'lganda Android telefonlarga bildirishnoma yetib bormaydi. FCM_SERVICE_ACCOUNT_FILE ni sozlang.")
	}
	return out
}

// LogSecurityChecks writes everything that is not fine to the log at startup.
func (s *Server) LogSecurityChecks() {
	for _, c := range s.SecurityChecks() {
		if c.Level == "ok" {
			continue
		}
		log.Printf("SECURITY [%s] %s: %s — %s", c.Level, c.Code, c.Title, c.Detail)
	}
}

// GET /admin/security-check
func (s *Server) handleAdminSecurityCheck(w http.ResponseWriter, _ *http.Request) {
	checks := s.SecurityChecks()
	ok := true
	for _, c := range checks {
		if c.Level == "critical" {
			ok = false
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": ok, "checks": checks})
}
