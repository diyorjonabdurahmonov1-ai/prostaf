package api

import (
	"net/http"

	"prostaff/internal/models"
	"prostaff/internal/sse"
	"prostaff/internal/store"
)

// GET /admin/users?query=  (?q= accepted as an alias)
func (s *Server) handleAdminUsers(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("query")
	if q == "" {
		q = r.URL.Query().Get("q")
	}
	users := s.store.ListUsers(q)
	if users == nil {
		users = []models.User{}
	}
	writeJSON(w, http.StatusOK, users)
}

// GET /admin/stats — real dashboard counters + 7-day commission series.
func (s *Server) handleAdminStats(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.store.AdminStats())
}

// GET /admin/orders?period=day|month|year&status=
func (s *Server) handleAdminOrders(w http.ResponseWriter, r *http.Request) {
	period := r.URL.Query().Get("period")
	status := r.URL.Query().Get("status")
	writeJSON(w, http.StatusOK, s.store.ListAllOrders(period, status))
}

// POST /admin/users/{id}/balance — body { amount } (negative debits).
func (s *Server) handleAdminBalance(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Amount int64 `json:"amount"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	wallet, err := s.store.AdminAdjustBalance(r.PathValue("id"), in.Amount)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, wallet)
}

// POST /admin/users/{id}/credit-limit — body { amount }. Works for companies
// too, unlike the worker-only endpoint.
func (s *Server) handleAdminUserCreditLimit(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Amount int64 `json:"amount"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	wallet, err := s.store.SetUserCreditLimit(r.PathValue("id"), in.Amount)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, wallet)
}

// POST /admin/workers — put a worker on the platform.
//
// Workers do not sign themselves up: the office takes the details over the
// phone and fills them in here. The account is created (or reused) for the
// given phone number, so the worker simply signs in and finds their panel.
func (s *Server) handleAdminCreateWorker(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Phone       string   `json:"phone"`
		Gmail       string   `json:"gmail"`
		Name        string   `json:"name"`
		FullName    string   `json:"full_name"`
		Gender      string   `json:"gender"`
		Age         int      `json:"age"`
		PriceMin    int64    `json:"price_min"`
		PriceMax    int64    `json:"price_max"`
		Lat         float64  `json:"lat"`
		Lng         float64  `json:"lng"`
		CategoryIDs []string `json:"category_ids"`
		CreditLimit int64    `json:"credit_limit"`
		Online      bool     `json:"online"`

		AvatarURL       string   `json:"avatar_url"`
		About           string   `json:"about"`
		ExperienceYears int      `json:"experience_years"`
		Address         string   `json:"address"`
		Images          []string `json:"images"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	user, err := s.store.AdminCreateWorker(store.AdminWorkerInput{
		Phone:           in.Phone,
		Gmail:           in.Gmail,
		Name:            in.Name,
		FullName:        in.FullName,
		Gender:          in.Gender,
		Age:             in.Age,
		PriceMin:        in.PriceMin,
		PriceMax:        in.PriceMax,
		Lat:             in.Lat,
		Lng:             in.Lng,
		CategoryIDs:     in.CategoryIDs,
		CreditLimit:     in.CreditLimit,
		Online:          in.Online,
		AvatarURL:       in.AvatarURL,
		About:           in.About,
		ExperienceYears: in.ExperienceYears,
		Address:         in.Address,
		Images:          in.Images,
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if user != nil {
		go s.notifyRoleChange(user.ID, user.Roles)
	}
	writeJSON(w, http.StatusCreated, user)
}

// notifyRoleChange fires a role.changed SSE event to the given user so the
// mobile app updates its panel list without requiring a manual refresh.
func (s *Server) notifyRoleChange(userID string, roles []string) {
	if roles == nil {
		roles = []string{}
	}
	s.publish(userID, sse.Event{
		Type: "role.changed",
		Data: map[string]any{"roles": roles},
	})
}

// PATCH /admin/workers/{id} — correct any field of a worker record.
func (s *Server) handleAdminUpdateWorker(w http.ResponseWriter, r *http.Request) {
	var in struct {
		FullName    *string   `json:"full_name"`
		Phone       *string   `json:"phone"`
		Gender      *string   `json:"gender"`
		Age         *int      `json:"age"`
		PriceMin    *int64    `json:"price_min"`
		PriceMax    *int64    `json:"price_max"`
		Lat         *float64  `json:"lat"`
		Lng         *float64  `json:"lng"`
		CategoryIDs *[]string `json:"category_ids"`

		AvatarURL       *string   `json:"avatar_url"`
		About           *string   `json:"about"`
		ExperienceYears *int      `json:"experience_years"`
		Address         *string   `json:"address"`
		Images          *[]string `json:"images"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	wp, err := s.store.AdminUpdateWorker(r.PathValue("id"), store.AdminWorkerPatch{
		FullName:        in.FullName,
		Phone:           in.Phone,
		Gender:          in.Gender,
		Age:             in.Age,
		PriceMin:        in.PriceMin,
		PriceMax:        in.PriceMax,
		Lat:             in.Lat,
		Lng:             in.Lng,
		CategoryIDs:     in.CategoryIDs,
		AvatarURL:       in.AvatarURL,
		About:           in.About,
		ExperienceYears: in.ExperienceYears,
		Address:         in.Address,
		Images:          in.Images,
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, wp)
}

// POST /admin/companies — put a service company on the platform.
func (s *Server) handleAdminCreateCompany(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Phone       string   `json:"phone"`
		Gmail       string   `json:"gmail"`
		Name        string   `json:"name"`
		Description string   `json:"description"`
		Images      []string `json:"images"`
		Price       int64    `json:"price"`
		CategoryIDs []string `json:"category_ids"`
		Lat         float64  `json:"lat"`
		Lng         float64  `json:"lng"`
		CreditLimit int64    `json:"credit_limit"`
		Online      bool     `json:"online"`

		AvatarURL       string `json:"avatar_url"`
		Address         string `json:"address"`
		WorkingHours    string `json:"working_hours"`
		EmployeesCount  int    `json:"employees_count"`
		ExperienceYears int    `json:"experience_years"`
		Email           string `json:"email"`
		Website         string `json:"website"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	user, err := s.store.AdminCreateCompany(store.AdminCompanyInput{
		Phone:           in.Phone,
		Gmail:           in.Gmail,
		Name:            in.Name,
		Description:     in.Description,
		Images:          in.Images,
		Price:           in.Price,
		CategoryIDs:     in.CategoryIDs,
		Lat:             in.Lat,
		Lng:             in.Lng,
		CreditLimit:     in.CreditLimit,
		Online:          in.Online,
		AvatarURL:       in.AvatarURL,
		Address:         in.Address,
		WorkingHours:    in.WorkingHours,
		EmployeesCount:  in.EmployeesCount,
		ExperienceYears: in.ExperienceYears,
		Email:           in.Email,
		Website:         in.Website,
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if user != nil {
		go s.notifyRoleChange(user.ID, user.Roles)
	}
	writeJSON(w, http.StatusCreated, user)
}

// POST /admin/workers/{id}/status — body { online }.
func (s *Server) handleAdminWorkerStatus(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Online bool `json:"online"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	wp, err := s.store.SetWorkerOnline(r.PathValue("id"), in.Online)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, wp)
}

// POST /admin/companies/{id}/status — body { online }.
func (s *Server) handleAdminCompanyStatus(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Online bool `json:"online"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	cp, err := s.store.SetCompanyOnline(r.PathValue("id"), in.Online)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cp)
}

// GET /admin/users/{id}
func (s *Server) handleAdminGetUser(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("id")
	u, ok := s.store.UserView(uid)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "user not found")
		return
	}
	writeJSON(w, http.StatusOK, u)
}

// PATCH /admin/users/{id}
func (s *Server) handleAdminUpdateUser(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name     *string `json:"name"`
		Gmail    *string `json:"gmail"`
		Active   *bool   `json:"active"`
		FullName *string `json:"full_name"`
		Phone    *string `json:"phone"`
		Age      *int    `json:"age"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	user, err := s.store.AdminUpdateUser(r.PathValue("id"), store.AdminUserPatch{
		Name:     in.Name,
		Gmail:    in.Gmail,
		Active:   in.Active,
		FullName: in.FullName,
		Phone:    in.Phone,
		Age:      in.Age,
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, user)
}

// POST /admin/users/{id}/roles
func (s *Server) handleAdminRoles(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Role   string `json:"role"`
		Action string `json:"action"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	user, err := s.store.ChangeRole(r.PathValue("id"), in.Role, in.Action)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	// Notify the user instantly so their app does not need a manual refresh.
	s.publish(user.ID, sse.Event{
		Type: "role.changed",
		Data: map[string]any{"roles": user.Roles},
	})
	writeJSON(w, http.StatusOK, user)
}

// POST /admin/users/{id}/push
func (s *Server) handleAdminPush(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("id")
	var in struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if _, ok := s.store.GetUser(userID); !ok {
		writeError(w, http.StatusNotFound, "not_found", "user not found")
		return
	}
	n := s.store.AddNotification(&models.Notification{
		UserID: userID,
		Type:   "admin.push",
		Title:  in.Title,
		Body:   in.Body,
	})
	s.publish(userID, sse.Event{Type: "admin.push", Data: map[string]any{"title": in.Title, "body": in.Body}})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "notification": n})
}

// POST /admin/workers/{id}/ban
func (s *Server) handleAdminBanWorker(w http.ResponseWriter, r *http.Request) {
	s.setWorkerBan(w, r, true)
}

// POST /admin/workers/{id}/unban
func (s *Server) handleAdminUnbanWorker(w http.ResponseWriter, r *http.Request) {
	s.setWorkerBan(w, r, false)
}

func (s *Server) setWorkerBan(w http.ResponseWriter, r *http.Request, banned bool) {
	wp, err := s.store.SetWorkerBanned(r.PathValue("id"), banned)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, wp)
}

// POST /admin/workers/{id}/credit-limit
func (s *Server) handleAdminCreditLimit(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Amount int64 `json:"amount"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	wp, err := s.store.SetWorkerCreditLimit(r.PathValue("id"), in.Amount)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, wp)
}

// GET /admin/companies
func (s *Server) handleAdminCompanies(w http.ResponseWriter, _ *http.Request) {
	companies := s.store.ListCompanies()
	if companies == nil {
		companies = []models.CompanyProfile{}
	}
	writeJSON(w, http.StatusOK, companies)
}

// POST /admin/companies/{id}/ban
func (s *Server) handleAdminBanCompany(w http.ResponseWriter, r *http.Request) {
	s.setCompanyBan(w, r, true)
}

// POST /admin/companies/{id}/unban
func (s *Server) handleAdminUnbanCompany(w http.ResponseWriter, r *http.Request) {
	s.setCompanyBan(w, r, false)
}

func (s *Server) setCompanyBan(w http.ResponseWriter, r *http.Request, banned bool) {
	cp, err := s.store.SetCompanyBanned(r.PathValue("id"), banned)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cp)
}

// PATCH /admin/companies/{id}
func (s *Server) handleAdminUpdateCompany(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name        *string   `json:"name"`
		Description *string   `json:"description"`
		Images      *[]string `json:"images"`
		Price       *int64    `json:"price"`
		Phone       *string   `json:"phone"`
		CategoryIDs *[]string `json:"category_ids"`
		Lat         *float64  `json:"lat"`
		Lng         *float64  `json:"lng"`

		AvatarURL       *string `json:"avatar_url"`
		Address         *string `json:"address"`
		WorkingHours    *string `json:"working_hours"`
		EmployeesCount  *int    `json:"employees_count"`
		ExperienceYears *int    `json:"experience_years"`
		Email           *string `json:"email"`
		Website         *string `json:"website"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	cp, err := s.store.UpdateCompanyAdmin(r.PathValue("id"), store.CompanyPatch{
		Name:            in.Name,
		Description:     in.Description,
		Images:          in.Images,
		Price:           in.Price,
		Phone:           in.Phone,
		CategoryIDs:     in.CategoryIDs,
		Lat:             in.Lat,
		Lng:             in.Lng,
		AvatarURL:       in.AvatarURL,
		Address:         in.Address,
		WorkingHours:    in.WorkingHours,
		EmployeesCount:  in.EmployeesCount,
		ExperienceYears: in.ExperienceYears,
		Email:           in.Email,
		Website:         in.Website,
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cp)
}

// GET /admin/analytics/orders?period=day|month|year
func (s *Server) handleAdminAnalytics(w http.ResponseWriter, r *http.Request) {
	period := r.URL.Query().Get("period")
	if period == "" {
		period = "day"
	}
	writeJSON(w, http.StatusOK, s.store.OrderAnalytics(period))
}

// GET /admin/finance
func (s *Server) handleAdminFinance(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.store.FinanceOverview())
}

// PATCH /admin/settings
func (s *Server) handleAdminSettings(w http.ResponseWriter, r *http.Request) {
	var in struct {
		CommissionPercent *int   `json:"commission_percent"`
		MinBalance        *int64 `json:"min_balance"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	settings := s.store.UpdateSettings(store.SettingsPatch{
		CommissionPercent: in.CommissionPercent,
		MinBalance:        in.MinBalance,
	})
	writeJSON(w, http.StatusOK, settings)
}
