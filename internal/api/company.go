package api

import (
	"net/http"
	"strconv"

	"prostaff/internal/models"
	"prostaff/internal/push"
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
	companies := s.store.NearbyCompanies(lat, lng, radius, userFrom(r).ID)
	if companies == nil {
		companies = []models.CompanyProfile{}
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
	writeJSON(w, http.StatusOK, cp)
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
	writeJSON(w, http.StatusOK, s.store.ListReviews(cp.UserID, 50))
}

// GET /company/orders (company's incoming orders)
func (s *Server) handleCompanyOrders(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	orders := s.store.ListCompanyOrders(u.ID)
	if orders == nil {
		orders = []models.Order{}
	}
	writeJSON(w, http.StatusOK, orders)
}

// POST /companies/{id}/order (direct order to a company)
func (s *Server) handleCompanyOrder(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	companyID := r.PathValue("id")
	var body struct {
		Description string  `json:"description"`
		Phone       string  `json:"phone"`
		Name        string  `json:"name"`
		ImageURL    string  `json:"image_url"`
		Lat         float64 `json:"lat"`
		Lng         float64 `json:"lng"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	order, err := s.store.CreateCompanyOrder(u.ID, companyID, store.CompanyOrderInput{
		Description: body.Description,
		Phone:       body.Phone,
		Name:        body.Name,
		ImageURL:    body.ImageURL,
		Lat:         body.Lat,
		Lng:         body.Lng,
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	// Notify the company owner in realtime.
	if company, ok := s.store.GetCompanyByID(companyID); ok {
		s.publish(company.UserID, sse.Event{Type: "order.status", Data: map[string]any{"order": order}})
		// Push notification so the company owner gets alerted even if the app is closed.
		if tok := s.store.GetPushToken(company.UserID); tok != "" {
			push.Send(push.Urgent(
				tok,
				"Yangi buyurtma!",
				order.Description,
				map[string]any{"type": "order.status", "order_id": order.ID},
				0,
			))
		}
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
	if accept {
		event = "order.accepted"
		title = "Firma buyurtmani qabul qildi"
	}
	s.publish(order.CustomerID, sse.Event{Type: event, Data: map[string]any{"order": order}})
	if tok := s.store.GetPushToken(order.CustomerID); tok != "" {
		push.Send(push.Urgent(tok, title, order.Description,
			map[string]any{"type": event, "order_id": order.ID}, 0))
	}
	writeJSON(w, http.StatusOK, order)
}
