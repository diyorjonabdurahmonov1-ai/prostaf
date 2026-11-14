package api

import (
	"fmt"
	"net/http"
	"strconv"

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
		Phone string `json:"phone"`
		Gmail string `json:"gmail"`
		Name  string `json:"name"`
		// PassportSeries is required: it is what the worker's three free jobs are
		// counted against, and it is unique across the platform.
		PassportSeries string   `json:"passport_series"`
		FullName       string   `json:"full_name"`
		Gender         string   `json:"gender"`
		Age            int      `json:"age"`
		PriceMin       int64    `json:"price_min"`
		PriceMax       int64    `json:"price_max"`
		Lat            float64  `json:"lat"`
		Lng            float64  `json:"lng"`
		CategoryIDs    []string `json:"category_ids"`
		CreditLimit    int64    `json:"credit_limit"`
		Online         bool     `json:"online"`

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
		PassportSeries:  in.PassportSeries,
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
		go s.notifyRoleChange(user.ID, user.Roles, models.RoleWorker, true)
	}
	writeJSON(w, http.StatusCreated, user)
}

// notifyRoleChange fires a role.changed event to the given user so the mobile
// app updates its panel list without requiring a manual refresh, and — when the
// change is one the person would want to hear about — tells them in words.
//
// The socket event alone was invisible: somebody the office signed up as a
// worker got a new tab in their app and no explanation of where it came from,
// and if their app was closed at the time, not even that until they next opened
// it. `changed` names the role that moved and `granted` says which way, so the
// message can be the one that actually happened rather than "something about
// your account is different".
func (s *Server) notifyRoleChange(userID string, roles []string, changed string, granted bool) {
	if roles == nil {
		roles = []string{}
	}
	s.publish(userID, sse.Event{
		Type: "role.changed",
		Data: map[string]any{"roles": roles},
	})

	var title, body string
	switch {
	case changed == models.RoleWorker && granted:
		title = "Siz ishchi bo'ldingiz"
		body = "Profilingiz ishchi sifatida faollashtirildi. Ilovadagi \"Ishchi\" panelidan buyurtmalarni qabul qilishingiz mumkin."
	case changed == models.RoleCompany && granted:
		title = "Siz firma bo'ldingiz"
		body = "Profilingiz firma sifatida faollashtirildi. Ilovadagi \"Firma\" panelidan buyurtmalarni qabul qilishingiz mumkin."
	case changed == models.RoleWorker && !granted:
		title = "Ishchi profili o'chirildi"
		body = "Endi ilovadan ishchi sifatida buyurtma qabul qila olmaysiz."
	case changed == models.RoleCompany && !granted:
		title = "Firma profili o'chirildi"
		body = "Endi ilovadan firma sifatida buyurtma qabul qila olmaysiz."
	default:
		// customer / admin: nothing a person needs to be woken up for.
		return
	}
	s.notify(userID, "role.changed", title, body,
		map[string]any{"role": changed, "granted": granted})
}

// PATCH /admin/workers/{id} — correct any field of a worker record.
func (s *Server) handleAdminUpdateWorker(w http.ResponseWriter, r *http.Request) {
	var in struct {
		PassportSeries *string   `json:"passport_series"`
		FullName       *string   `json:"full_name"`
		Phone          *string   `json:"phone"`
		Gender         *string   `json:"gender"`
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
		PassportSeries:  in.PassportSeries,
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
		Phone string `json:"phone"`
		Gmail string `json:"gmail"`
		Name  string `json:"name"`
		// INN is required, and SubscriptionMonths optionally sells the firm its
		// first months in the same call — a firm that paid at the counter should
		// not have to be opened and then topped up in two steps.
		INN                string   `json:"inn"`
		SubscriptionMonths int      `json:"subscription_months"`
		Description        string   `json:"description"`
		Images             []string `json:"images"`
		Price              int64    `json:"price"`
		CategoryIDs        []string `json:"category_ids"`
		Services           string   `json:"services"`
		Lat                float64  `json:"lat"`
		Lng                float64  `json:"lng"`
		CreditLimit        int64    `json:"credit_limit"`
		Online             bool     `json:"online"`

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
		Phone:              in.Phone,
		Gmail:              in.Gmail,
		Name:               in.Name,
		INN:                in.INN,
		SubscriptionMonths: in.SubscriptionMonths,

		Description:     in.Description,
		Images:          in.Images,
		Price:           in.Price,
		CategoryIDs:     in.CategoryIDs,
		Services:        in.Services,
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
		go s.notifyRoleChange(user.ID, user.Roles, models.RoleCompany, true)
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
	// Notify the user instantly so their app does not need a manual refresh —
	// and tell them what changed, which the socket event on its own never did.
	s.notifyRoleChange(user.ID, user.Roles, in.Role, in.Action == "add")
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
	// Stored, socketed *and* pushed. This used to do the first two only, so an
	// operator's message reached a phone that happened to be open and nobody
	// else's.
	s.publish(userID, sse.Event{Type: "admin.push", Data: map[string]any{"title": in.Title, "body": in.Body}})
	n := s.notify(userID, "admin.push", in.Title, in.Body, nil)
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
		INN         *string   `json:"inn"`
		CategoryIDs *[]string `json:"category_ids"`
		Services    *string   `json:"services"`
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
		INN:             in.INN,
		CategoryIDs:     in.CategoryIDs,
		Services:        in.Services,
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

// The service list.
//
// Every order names a category and every provider claims a few, which makes this
// the one table the whole platform is indexed by — and the one the office needs
// to be able to edit without a deploy. See store/categories.go for why a used
// category is retired rather than deleted.

// GET /admin/categories — the whole list, retired rows included, each carrying
// what it is holding so the panel knows what may be deleted.
func (s *Server) handleAdminCategories(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.store.CategoriesWithUsage())
}

// POST /admin/categories
func (s *Server) handleAdminCreateCategory(w http.ResponseWriter, r *http.Request) {
	var in struct {
		// Key is the stable slug. Left out it is derived from the Uzbek name,
		// which is what the office types and what the seeded rows were built from.
		Key    string `json:"key"`
		NameUz string `json:"name_uz"`
		NameRu string `json:"name_ru"`
		NameEn string `json:"name_en"`
		Icon   string `json:"icon"`
		Active *bool  `json:"active"`
		Sort   *int   `json:"sort"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	c, err := s.store.CreateCategory(store.CategoryInput{
		Key:    in.Key,
		NameUz: in.NameUz,
		NameRu: in.NameRu,
		NameEn: in.NameEn,
		Icon:   in.Icon,
		Active: in.Active,
		Sort:   in.Sort,
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

// PATCH /admin/categories/{id}
//
// Renaming is safe at any time: the id is what orders and providers point at, so
// a corrected name simply starts showing up everywhere it was already used.
func (s *Server) handleAdminUpdateCategory(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Key    *string `json:"key"`
		NameUz *string `json:"name_uz"`
		NameRu *string `json:"name_ru"`
		NameEn *string `json:"name_en"`
		Icon   *string `json:"icon"`
		Active *bool   `json:"active"`
		Sort   *int    `json:"sort"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	c, err := s.store.UpdateCategory(r.PathValue("id"), store.CategoryPatch{
		Key:    in.Key,
		NameUz: in.NameUz,
		NameRu: in.NameRu,
		NameEn: in.NameEn,
		Icon:   in.Icon,
		Active: in.Active,
		Sort:   in.Sort,
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// DELETE /admin/categories/{id} — only for a category nothing points at.
// Anything in use comes back as category_in_use; the fix is to retire it.
func (s *Server) handleAdminDeleteCategory(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteCategory(r.PathValue("id")); err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// The tariff cards.
//
// A firm's plans differ in length and nothing else — every subscribed firm gets
// the same platform — so the office sets a price per term and the app renders
// them as cards. There is no monthly rate being multiplied out: "3 oy — 200 000"
// is a number somebody decided, not one arithmetic derived.

// GET /admin/subscription-plans — every tariff, including retired ones.
func (s *Server) handleAdminPlans(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.store.ListSubscriptionPlans(false))
}

// POST /admin/subscription-plans
func (s *Server) handleAdminCreatePlan(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Months int    `json:"months"`
		Price  int64  `json:"price"`
		Title  string `json:"title"`
		// OldPrice is the struck-through figure on the card. Presentational and
		// never charged.
		OldPrice int64 `json:"old_price"`
		Active   *bool `json:"active"`
		Sort     *int  `json:"sort"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	plan, err := s.store.CreateSubscriptionPlan(store.PlanInput{
		Months:   in.Months,
		Price:    in.Price,
		OldPrice: in.OldPrice,
		Title:    in.Title,
		Active:   in.Active,
		Sort:     in.Sort,
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, plan)
}

// PATCH /admin/subscription-plans/{id}
func (s *Server) handleAdminUpdatePlan(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Months   *int    `json:"months"`
		Price    *int64  `json:"price"`
		OldPrice *int64  `json:"old_price"`
		Title    *string `json:"title"`
		Active   *bool   `json:"active"`
		Sort     *int    `json:"sort"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	plan, err := s.store.UpdateSubscriptionPlan(r.PathValue("id"), store.PlanPatch{
		Months:   in.Months,
		Price:    in.Price,
		OldPrice: in.OldPrice,
		Title:    in.Title,
		Active:   in.Active,
		Sort:     in.Sort,
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

// DELETE /admin/subscription-plans/{id}
//
// Safe at any time: a plan is only read to price a checkout, and the months a
// firm already bought live on its own record. Nothing points back at this row.
func (s *Server) handleAdminDeletePlan(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteSubscriptionPlan(r.PathValue("id")); err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// GET /admin/settings — the current platform values.
//
// The panel used to read the commission out of /admin/finance and had nowhere at
// all to read the rest, so a field it could not fetch was a field it rendered
// blank and silently overwrote on save.
func (s *Server) handleAdminGetSettings(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.store.GetSettings())
}

// PATCH /admin/settings
func (s *Server) handleAdminSettings(w http.ResponseWriter, r *http.Request) {
	var in struct {
		CommissionPercent *int   `json:"commission_percent"`
		MinBalance        *int64 `json:"min_balance"`
		// How many jobs a new provider gets free, what a firm's month costs, the
		// standing discount on it, and how much notice a firm gets before its
		// subscription runs out. All four are the office's to set.
		FreeOrderLimit         *int   `json:"free_order_limit"`
		CompanyMonthlyPrice    *int64 `json:"company_monthly_price"`
		CompanyDiscountPercent *int   `json:"company_discount_percent"`
		SubscriptionWarnDays   *int   `json:"subscription_warn_days"`
		// How far a worker may be from a job and still be rung, and how many
		// are rung per wave, nearest first (0 = everybody at once).
		MatchRadiusKm    *int `json:"match_radius_km"`
		DispatchWaveSize *int `json:"dispatch_wave_size"`
		// How long a customer waits before reporting a no-show, and the
		// oldest app version still allowed with where to get a newer one.
		NoShowMinutes   *int    `json:"no_show_minutes"`
		MinAppVersion   *string `json:"min_app_version"`
		AndroidStoreURL *string `json:"android_store_url"`
		IOSStoreURL     *string `json:"ios_store_url"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	settings := s.store.UpdateSettings(store.SettingsPatch{
		CommissionPercent:      in.CommissionPercent,
		MinBalance:             in.MinBalance,
		FreeOrderLimit:         in.FreeOrderLimit,
		CompanyMonthlyPrice:    in.CompanyMonthlyPrice,
		CompanyDiscountPercent: in.CompanyDiscountPercent,
		SubscriptionWarnDays:   in.SubscriptionWarnDays,
		MatchRadiusKm:          in.MatchRadiusKm,
		DispatchWaveSize:       in.DispatchWaveSize,
		NoShowMinutes:          in.NoShowMinutes,
		MinAppVersion:          in.MinAppVersion,
		AndroidStoreURL:        in.AndroidStoreURL,
		IOSStoreURL:            in.IOSStoreURL,
	})
	writeJSON(w, http.StatusOK, settings)
}

// POST /admin/companies/{id}/subscription — body { months, price_per_month?,
// discount_percent? }. Sells a firm more months.
//
// Price and discount are optional: leaving them out uses the current settings,
// which is the normal case. Sending them is how the office gives one firm a
// deal without moving the price for everybody.
func (s *Server) handleAdminExtendSubscription(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Months          int    `json:"months"`
		PricePerMonth   *int64 `json:"price_per_month"`
		DiscountPercent *int   `json:"discount_percent"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	sub, cp, err := s.store.ExtendCompanySubscription(r.PathValue("id"), store.SubscriptionExtension{
		Months:          in.Months,
		PricePerMonth:   in.PricePerMonth,
		DiscountPercent: in.DiscountPercent,
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	// The firm is told, and its app is told, so the banner that has been telling
	// it to renew disappears without a manual refresh.
	if cp != nil {
		s.publish(cp.UserID, sse.Event{
			Type: "subscription.changed",
			Data: map[string]any{"company": cp},
		})
		until := ""
		if cp.SubscriptionUntil != nil {
			until = cp.SubscriptionUntil.Format("02.01.2006")
		}
		s.notify(cp.UserID, "subscription.extended",
			"Obuna faollashtirildi",
			fmt.Sprintf("Hisobingiz %d oyga uzaytirildi. Amal qilish muddati: %s.", sub.Months, until),
			map[string]any{"months": sub.Months, "until": until, "amount": sub.Amount})
	}
	writeJSON(w, http.StatusOK, map[string]any{"subscription": sub, "company": cp})
}

// GET /admin/companies/{id}/subscriptions — what this firm has bought.
func (s *Server) handleAdminSubscriptions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.store.ListCompanySubscriptions(r.PathValue("id")))
}

// GET /admin/payments?status=&purpose=&limit= — the payment log.
//
// Every attempt, not just the ones that worked, and each carrying whatever went
// wrong with it in words. Before this the office could see money that had
// arrived and nothing at all about money that had not: a firm ringing up to say
// "I paid and nothing happened" was a conversation with no evidence on our side
// of it.
//
// `status=problem` is the filter that matters — failures plus anything that
// settled with a note on it.
func (s *Server) handleAdminPayments(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	writeJSON(w, http.StatusOK, map[string]any{
		"payments": s.store.ListAllPayments(q.Get("status"), q.Get("purpose"), limit),
		"totals":   s.store.PaymentOverview(),
	})
}

// GET /admin/subscriptions — every tariff ever sold, newest first.
//
// The office's answer to "who is paying us, and who is about to stop": the
// history alongside the two counters that decide whether anybody needs ringing
// this week.
func (s *Server) handleAdminAllSubscriptions(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	writeJSON(w, http.StatusOK, map[string]any{
		"subscriptions": s.store.ListAllSubscriptions(limit),
		"totals":        s.store.SubscriptionOverview(),
	})
}

// POST /admin/users/{id}/free-orders — body { role, used }. Sets how many trial
// jobs the account has spent.
//
// The counter hangs off the identity document, not the account, precisely so it
// cannot be reset by re-creating a profile — which means an honest mistake (a
// trial burned on a test order) needs a deliberate way back. This is it.
func (s *Server) handleAdminFreeOrders(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Role string `json:"role"`
		Used int    `json:"used"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.Role == "" {
		in.Role = models.RoleWorker
	}
	key := s.store.TrialKeyForUser(r.PathValue("id"), in.Role)
	if key == "" {
		writeError(w, http.StatusNotFound, "not_found",
			"Bu foydalanuvchida hujjat raqami yo'q — passport seriyasi yoki INN kiritilmagan.")
		return
	}
	s.store.SetFreeOrdersUsed(key, in.Used)
	u, _ := s.store.UserView(r.PathValue("id"))
	writeJSON(w, http.StatusOK, u)
}
