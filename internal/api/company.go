package api

import (
	"net/http"
	"strconv"

	"prostaff/internal/models"
	"prostaff/internal/sse"
	"prostaff/internal/store"
)

// PATCH /company/profile (the company edits its own listing; the account itself
// is created by an admin via POST /admin/companies)
func (s *Server) handlePatchCompanyProfile(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	var body struct {
		Name        *string   `json:"name"`
		Description *string   `json:"description"`
		Images      *[]string `json:"images"`
		Price       *int64    `json:"price"`
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
	if !decodeJSON(w, r, &body) {
		return
	}
	cp, err := s.store.UpdateCompanyProfile(u.ID, store.CompanyPatch{
		Name:            body.Name,
		Description:     body.Description,
		Images:          body.Images,
		Price:           body.Price,
		CategoryIDs:     body.CategoryIDs,
		Lat:             body.Lat,
		Lng:             body.Lng,
		AvatarURL:       body.AvatarURL,
		Address:         body.Address,
		WorkingHours:    body.WorkingHours,
		EmployeesCount:  body.EmployeesCount,
		ExperienceYears: body.ExperienceYears,
		Email:           body.Email,
		Website:         body.Website,
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cp)
}

// POST /company/status
func (s *Server) handleCompanyStatus(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	var body struct {
		Online bool `json:"online"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	cp, err := s.store.SetCompanyStatus(u.ID, body.Online)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cp)
}

// GET /companies?lat=&lng=&radius_km= (bookable online companies; the point is
// optional and narrows the list to firms near the customer)
func (s *Server) handleListCompanies(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	lat, _ := strconv.ParseFloat(q.Get("lat"), 64)
	lng, _ := strconv.ParseFloat(q.Get("lng"), 64)
	radius, _ := strconv.ParseFloat(q.Get("radius_km"), 64)
	u := userFrom(r)
	found := s.store.NearbyCompanies(lat, lng, radius, u.ID)
	companies := make([]models.CompanyProfile, 0, len(found))
	for _, cp := range found {
		companies = append(companies, s.publicCompany(u, cp))
	}
	writeJSON(w, http.StatusOK, companies)
}

// GET /companies/{id} (public company profile)
func (s *Server) handleGetCompany(w http.ResponseWriter, r *http.Request) {
	cp, ok := s.store.GetCompanyByID(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "company not found")
		return
	}
	writeJSON(w, http.StatusOK, s.publicCompany(userFrom(r), *cp))
}

// publicCompany is publicWorker for a firm, and for the same reason: a listing
// of reachable providers is the product, and printing the number on the profile
// gives it away to anyone who scrolls past.
//
// The number is on the order once the firm accepts, which is where the customer
// reads it — this only keeps it off the profile page.
// The INN and the firm's billing state are stripped for the same reason a
// worker's passport is: what a firm pays the platform, and when its month runs
// out, is between the firm and the office. A customer picks a firm on its work,
// not on its invoices.
func (s *Server) publicCompany(viewer *models.User, cp models.CompanyProfile) models.CompanyProfile {
	if viewer != nil && (cp.UserID == viewer.ID || viewer.HasRole(models.RoleAdmin)) {
		return cp
	}
	cp.INN = ""
	cp.SubscriptionUntil = nil
	cp.SubscriptionDaysLeft = 0
	cp.FreeOrdersUsed = 0
	cp.FreeOrdersLimit = 0
	if viewer == nil {
		cp.Phone = ""
		return cp
	}
	if s.store.HasHiredCompany(viewer.ID, cp.ID) {
		return cp
	}
	cp.Phone = ""
	return cp
}

// GET /companies/{id}/reviews — what customers wrote about this firm.
//
// The mirror of the worker route: kept off the profile for the same reason, and
// `{id}` is likewise the company profile id while ratings are keyed by account.
func (s *Server) handleCompanyReviews(w http.ResponseWriter, r *http.Request) {
	cp, ok := s.store.GetCompanyByID(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "company not found")
		return
	}
	writeJSON(w, http.StatusOK, s.store.ListReviews(cp.UserID, models.RoleCompany, 50))
}

// GET /company/orders (company's incoming orders)
func (s *Server) handleCompanyOrders(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	orders := s.store.ListCompanyOrders(u.ID)
	if orders == nil {
		orders = []models.Order{}
	}
	// The number belongs to the firm only once it has taken the job — the same
	// rule a worker has always been held to. Masked here rather than on the
	// screen: a list endpoint that hands out phone numbers is one `curl` away
	// from being a directory of everyone who ever ordered.
	for i := range orders {
		orders[i] = maskForCompany(orders[i])
	}
	writeJSON(w, http.StatusOK, orders)
}

// POST /companies/{id}/order (direct order to a company)
func (s *Server) handleCompanyOrder(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	companyID := r.PathValue("id")
	var body struct {
		Description string   `json:"description"`
		Phone       string   `json:"phone"`
		Name        string   `json:"name"`
		Images      []string `json:"images"`
		ImageURL    string   `json:"image_url"`
		Lat         float64  `json:"lat"`
		Lng         float64  `json:"lng"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	order, err := s.store.CreateCompanyOrder(u.ID, companyID, store.CompanyOrderInput{
		Description: body.Description,
		Phone:       body.Phone,
		Name:        body.Name,
		Images:      body.Images,
		ImageURL:    body.ImageURL,
		Lat:         body.Lat,
		Lng:         body.Lng,
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	// Notify the company owner in realtime, and leave the message behind so a
	// firm that missed the banner still finds the job in their notifications.
	if company, ok := s.store.GetCompanyByID(companyID); ok {
		// Masked: this is the firm's first sight of the job and it has agreed to
		// nothing yet. An unmasked socket frame would put the number on the card
		// no matter what the REST endpoints withhold.
		offered := maskForCompany(*order)
		s.publish(company.UserID, sse.Event{Type: "order.status", Data: map[string]any{"order": offered}})
		s.notify(company.UserID, "order.new", "Yangi buyurtma!", order.Description,
			map[string]any{"order_id": order.ID})
	}
	writeJSON(w, http.StatusCreated, order)
}

// POST /company/orders/{id}/accept
func (s *Server) handleCompanyAccept(w http.ResponseWriter, r *http.Request) {
	s.companyRespond(w, r, true)
}

// POST /company/orders/{id}/reject
func (s *Server) handleCompanyReject(w http.ResponseWriter, r *http.Request) {
	s.companyRespond(w, r, false)
}

// A company answers an order directly — it never searches for workers.
func (s *Server) companyRespond(w http.ResponseWriter, r *http.Request, accept bool) {
	u := userFrom(r)
	order, err := s.store.CompanyRespond(r.PathValue("id"), u.ID, accept)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	event := "order.status"
	title := "Buyurtma bekor qilindi"
	body := order.Description
	if accept {
		event = "order.accepted"
		title = "Firma buyurtmani qabul qildi"
		if order.WorkerPhone != "" {
			body = "Telefon raqami: " + order.WorkerPhone
		}
	}
	s.publish(order.CustomerID, sse.Event{Type: event, Data: map[string]any{"order": order}})
	s.notify(order.CustomerID, event, title, body, map[string]any{"order_id": order.ID})
	// A firm on its trial jobs is told the moment the last one is spent — the
	// next order it is offered is one it cannot take without a subscription.
	if accept {
		if cp, ok := s.store.CompanyByUser(u.ID); ok && !cp.SubscriptionActive {
			s.notifyTrialSpent(u.ID, cp.FreeOrdersLeft(), models.RoleCompany)
		}
	}
	// Accepting reveals the number; declining leaves it hidden, which is the
	// point of hiding it in the first place.
	writeJSON(w, http.StatusOK, maskForCompany(*order))
}
