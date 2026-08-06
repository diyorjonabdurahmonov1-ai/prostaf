package store

import (
	"math"
	"sort"
	"time"

	"prostaff/internal/geo"
	"prostaff/internal/id"
	"prostaff/internal/models"
)

// WorkerInput is the first-time worker profile creation payload.
type WorkerInput struct {
	FullName    string
	Phone       string
	Gender      string
	Age         int
	PriceMin    int64
	PriceMax    int64
	Lat         float64
	Lng         float64
	CategoryIDs []string
	// The public half of the record — everything a customer reads before they
	// let this person into their home.
	AvatarURL       string
	About           string
	ExperienceYears int
	Address         string
	Images          []string
}

func (s *Store) scanWorker(row interface{ Scan(dest ...any) error }) (*models.WorkerProfile, error) {
	var wp models.WorkerProfile
	var catIDs, images string
	err := row.Scan(&wp.ID, &wp.UserID, &wp.FullName, &wp.Phone, &wp.Gender, &wp.Age,
		&wp.PriceMin, &wp.PriceMax, &wp.Lat, &wp.Lng, &catIDs,
		&wp.Online, &wp.Banned, &wp.CreditLimit, &wp.Rating, &wp.RatingsCount, &wp.CreatedAt,
		&wp.AvatarURL, &wp.About, &wp.ExperienceYears, &wp.Address, &images)
	if err != nil {
		return nil, err
	}
	wp.CategoryIDs = unmarshalStrings(catIDs)
	wp.Images = unmarshalStrings(images)
	return &wp, nil
}

// workerCols is every column a WorkerProfile is scanned from, in scanWorker's
// order. Every query using it reads `FROM workers`, which is what lets the
// avatar subquery below correlate.
//
// The avatar falls back to the account photo when the worker has not set one of
// their own. The two stay separate on purpose — the face a customer studies
// before opening their door is the worker profile, not whatever the account
// holder picked — but "not set" should mean "use the photo we do have" rather
// than showing a customer initials while the worker sees their photo on their
// own profile tab.
const workerCols = `id, user_id, full_name, phone, gender, age, price_min, price_max, lat, lng,
	category_ids, online, banned, credit_limit, rating, ratings_count, created_at,
	COALESCE(NULLIF(avatar_url, ''), (SELECT u.avatar_url FROM users u WHERE u.id = workers.user_id), '') AS avatar_url,
	about, experience_years, address, images`

// CreateWorkerProfile creates a worker profile and grants the worker role.
func (s *Store) CreateWorkerProfile(userID string, in WorkerInput) (*models.WorkerProfile, error) {
	u, ok := s.GetUser(userID)
	if !ok {
		return nil, ErrNotFound
	}
	// Check if already exists.
	var existing int
	s.db.QueryRow("SELECT COUNT(*) FROM workers WHERE user_id=$1", userID).Scan(&existing)
	if existing > 0 {
		return nil, ErrConflict
	}
	if in.FullName == "" || in.Phone == "" {
		return nil, ErrBadRequest
	}
	for _, c := range in.CategoryIDs {
		if !s.categoryExists(c) {
			return nil, ErrBadRequest
		}
	}
	gender := in.Gender
	if gender != "male" && gender != "female" {
		gender = "male"
	}
	wp := &models.WorkerProfile{
		ID:              id.New(),
		UserID:          userID,
		FullName:        in.FullName,
		Phone:           in.Phone,
		Gender:          gender,
		Age:             in.Age,
		PriceMin:        in.PriceMin,
		PriceMax:        in.PriceMax,
		Lat:             in.Lat,
		Lng:             in.Lng,
		CategoryIDs:     in.CategoryIDs,
		Online:          false,
		AvatarURL:       in.AvatarURL,
		About:           in.About,
		ExperienceYears: in.ExperienceYears,
		Address:         in.Address,
		Images:          in.Images,
		CreatedAt:       time.Now(),
	}
	if wp.Images == nil {
		wp.Images = []string{}
	}
	s.db.Exec(`INSERT INTO workers (id, user_id, full_name, phone, gender, age, price_min, price_max,
		lat, lng, category_ids, created_at, avatar_url, about, experience_years, address, images)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`,
		wp.ID, wp.UserID, wp.FullName, wp.Phone, wp.Gender, wp.Age, wp.PriceMin, wp.PriceMax, wp.Lat, wp.Lng,
		marshalJSON(wp.CategoryIDs), wp.CreatedAt,
		wp.AvatarURL, wp.About, wp.ExperienceYears, wp.Address, marshalJSON(wp.Images))
	// Grant worker role.
	_ = u
	s.addRoleInternal(userID, models.RoleWorker)
	return wp, nil
}

// WorkerPatch holds the worker-editable fields. Name, phone and age stay with
// the admin who opened the account; how they present themselves is theirs.
type WorkerPatch struct {
	PriceMin    *int64
	PriceMax    *int64
	CategoryIDs *[]string

	AvatarURL       *string
	About           *string
	ExperienceYears *int
	Address         *string
	Images          *[]string
}

// UpdateWorkerProfile applies a worker-initiated patch.
func (s *Store) UpdateWorkerProfile(userID string, p WorkerPatch) (*models.WorkerProfile, error) {
	wp, ok := s.GetWorkerByUser(userID)
	if !ok {
		return nil, ErrNotFound
	}
	if p.PriceMin != nil {
		wp.PriceMin = *p.PriceMin
	}
	if p.PriceMax != nil {
		wp.PriceMax = *p.PriceMax
	}
	if p.CategoryIDs != nil {
		for _, c := range *p.CategoryIDs {
			if !s.categoryExists(c) {
				return nil, ErrBadRequest
			}
		}
		wp.CategoryIDs = *p.CategoryIDs
	}
	applyWorkerPresentation(wp, p.AvatarURL, p.About, p.ExperienceYears, p.Address, p.Images)
	s.db.Exec(`UPDATE workers SET price_min=$1, price_max=$2, category_ids=$3, avatar_url=$4,
		about=$5, experience_years=$6, address=$7, images=$8 WHERE user_id=$9`,
		wp.PriceMin, wp.PriceMax, marshalJSON(wp.CategoryIDs), wp.AvatarURL, wp.About,
		wp.ExperienceYears, wp.Address, marshalJSON(wp.Images), userID)
	return wp, nil
}

// applyWorkerPresentation copies the public-profile fields onto wp when they
// were sent. Shared by the worker's own patch and the admin's, so the two can
// never drift apart on what a photo or a bio means.
func applyWorkerPresentation(wp *models.WorkerProfile, avatar, about *string, years *int, address *string, images *[]string) {
	if avatar != nil {
		wp.AvatarURL = *avatar
	}
	if about != nil {
		wp.About = *about
	}
	if years != nil && *years >= 0 {
		wp.ExperienceYears = *years
	}
	if address != nil {
		wp.Address = *address
	}
	if images != nil {
		wp.Images = *images
	}
	if wp.Images == nil {
		wp.Images = []string{}
	}
}

// SetWorkerStatus toggles availability and optionally updates location.
func (s *Store) SetWorkerStatus(userID string, online bool, lat, lng *float64) (*models.WorkerProfile, error) {
	wp, ok := s.GetWorkerByUser(userID)
	if !ok {
		return nil, ErrNotFound
	}
	wp.Online = online
	if lat != nil {
		wp.Lat = *lat
	}
	if lng != nil {
		wp.Lng = *lng
	}
	s.db.Exec("UPDATE workers SET online=$1, lat=$2, lng=$3 WHERE user_id=$4",
		wp.Online, wp.Lat, wp.Lng, userID)
	return wp, nil
}

// GetWorkerByUser returns a worker profile by user id.
func (s *Store) GetWorkerByUser(userID string) (*models.WorkerProfile, bool) {
	wp, err := s.scanWorker(s.db.QueryRow("SELECT "+workerCols+" FROM workers WHERE user_id=$1", userID))
	if err != nil {
		return nil, false
	}
	return wp, true
}

// NearbyWorkers returns bookable workers within radiusKm of a point, nearest
// first, each carrying its distance: online, not banned, not already on a job,
// and never the caller themselves. Per-order matchmaking goes through Matches.
func (s *Store) NearbyWorkers(lat, lng, radiusKm float64, excludeUserID string) []models.WorkerProfile {
	if radiusKm <= 0 {
		radiusKm = matchRadiusKm
	}
	rows, err := s.db.Query("SELECT " + workerCols + " FROM workers WHERE online=TRUE AND banned=FALSE")
	if err != nil {
		return []models.WorkerProfile{}
	}
	defer rows.Close()

	out := []models.WorkerProfile{}
	for rows.Next() {
		wp, err := s.scanWorker(rows)
		if err != nil {
			continue
		}
		if wp.UserID == excludeUserID || s.WorkerIsBusy(wp.UserID) {
			continue
		}
		d := geo.DistanceKm(lat, lng, wp.Lat, wp.Lng)
		if d > radiusKm {
			continue
		}
		dist := d
		wp.DistanceKm = &dist
		out = append(out, *wp)
	}
	sort.Slice(out, func(i, j int) bool { return *out[i].DistanceKm < *out[j].DistanceKm })
	return out
}

// GetWorkerByID returns a worker profile by profile id.
func (s *Store) GetWorkerByID(workerID string) (*models.WorkerProfile, bool) {
	wp, err := s.scanWorker(s.db.QueryRow("SELECT "+workerCols+" FROM workers WHERE id=$1", workerID))
	if err != nil {
		return nil, false
	}
	return wp, true
}

// WorkerUserID returns the user ID for a worker profile ID.
func (s *Store) WorkerUserID(workerProfileID string) (string, bool) {
	var uid string
	err := s.db.QueryRow("SELECT user_id FROM workers WHERE id=$1", workerProfileID).Scan(&uid)
	if err != nil {
		return "", false
	}
	return uid, true
}

// BanWorker bans a worker.
func (s *Store) BanWorker(userID string) error {
	res, _ := s.db.Exec("UPDATE workers SET banned=TRUE, online=FALSE WHERE user_id=$1", userID)
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// UnbanWorker unbans a worker.
func (s *Store) UnbanWorker(userID string) error {
	res, _ := s.db.Exec("UPDATE workers SET banned=FALSE WHERE user_id=$1", userID)
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetCreditLimit sets a worker's credit limit.
func (s *Store) SetCreditLimit(userID string, limit int64) error {
	res, _ := s.db.Exec("UPDATE workers SET credit_limit=$1 WHERE user_id=$2", limit, userID)
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- Company ----------------------------------------------------------------

type CompanyInput struct {
	Name        string
	Phone       string
	Description string
	Images      []string
	Price       int64
	CategoryIDs []string
	Lat         float64
	Lng         float64

	AvatarURL       string
	Address         string
	WorkingHours    string
	EmployeesCount  int
	ExperienceYears int
	Email           string
	Website         string
}

func (s *Store) scanCompany(row interface{ Scan(dest ...any) error }) (*models.CompanyProfile, error) {
	var cp models.CompanyProfile
	var images, categoryIDs string
	err := row.Scan(&cp.ID, &cp.UserID, &cp.Name, &cp.Phone, &cp.Description, &images,
		&cp.Online, &cp.Banned, &cp.Rating, &cp.RatingsCount, &cp.CreatedAt, &cp.Price,
		&cp.AvatarURL, &cp.Address, &cp.WorkingHours, &cp.EmployeesCount, &cp.ExperienceYears,
		&cp.Email, &cp.Website, &categoryIDs, &cp.Lat, &cp.Lng)
	if err != nil {
		return nil, err
	}
	cp.Images = unmarshalStrings(images)
	cp.CategoryIDs = unmarshalStrings(categoryIDs)
	return &cp, nil
}

const companyCols = "id, user_id, name, phone, description, images, online, banned, rating, ratings_count, created_at, price, avatar_url, address, working_hours, employees_count, experience_years, email, website, category_ids, lat, lng"

// CreateCompanyProfile creates a company profile and grants the company role.
func (s *Store) CreateCompanyProfile(userID string, in CompanyInput) (*models.CompanyProfile, error) {
	_, ok := s.GetUser(userID)
	if !ok {
		return nil, ErrNotFound
	}
	var existing int
	s.db.QueryRow("SELECT COUNT(*) FROM companies WHERE user_id=$1", userID).Scan(&existing)
	if existing > 0 {
		return nil, ErrConflict
	}
	// A company must publish what it charges — customers order at that price.
	if in.Name == "" || in.Price <= 0 {
		return nil, ErrBadRequest
	}
	// A firm that lists a category nobody has heard of is a firm no search
	// finds, so a bad id is refused rather than quietly stored.
	for _, c := range in.CategoryIDs {
		if !s.categoryExists(c) {
			return nil, ErrBadRequest
		}
	}
	cp := &models.CompanyProfile{
		ID:              id.New(),
		UserID:          userID,
		Name:            in.Name,
		Phone:           in.Phone,
		Description:     in.Description,
		Images:          in.Images,
		Price:           in.Price,
		CategoryIDs:     in.CategoryIDs,
		Lat:             in.Lat,
		Lng:             in.Lng,
		Online:          false,
		AvatarURL:       in.AvatarURL,
		Address:         in.Address,
		WorkingHours:    in.WorkingHours,
		EmployeesCount:  in.EmployeesCount,
		ExperienceYears: in.ExperienceYears,
		Email:           in.Email,
		Website:         in.Website,
		CreatedAt:       time.Now(),
	}
	if cp.Images == nil {
		cp.Images = []string{}
	}
	if cp.CategoryIDs == nil {
		cp.CategoryIDs = []string{}
	}
	s.db.Exec(`INSERT INTO companies (id, user_id, name, phone, description, images, created_at, price,
		avatar_url, address, working_hours, employees_count, experience_years, email, website,
		category_ids, lat, lng)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`,
		cp.ID, cp.UserID, cp.Name, cp.Phone, cp.Description,
		marshalJSON(cp.Images), cp.CreatedAt, cp.Price,
		cp.AvatarURL, cp.Address, cp.WorkingHours, cp.EmployeesCount, cp.ExperienceYears,
		cp.Email, cp.Website, marshalJSON(cp.CategoryIDs), cp.Lat, cp.Lng)
	s.addRoleInternal(userID, models.RoleCompany)
	return cp, nil
}

type CompanyPatch struct {
	Name        *string
	Description *string
	Images      *[]string
	Price       *int64
	CategoryIDs *[]string
	Lat         *float64
	Lng         *float64
	// Phone is admin-only: the company's contact number is part of the record
	// the office fills in, not something the account edits itself.
	Phone *string

	AvatarURL       *string
	Address         *string
	WorkingHours    *string
	EmployeesCount  *int
	ExperienceYears *int
	Email           *string
	Website         *string
}

// apply copies every field that was sent onto cp. Phone is deliberately left
// out — it is admin-only and each caller decides for itself.
func (p CompanyPatch) apply(cp *models.CompanyProfile) {
	if p.Name != nil {
		cp.Name = *p.Name
	}
	if p.Description != nil {
		cp.Description = *p.Description
	}
	if p.Images != nil {
		cp.Images = *p.Images
	}
	if p.Price != nil && *p.Price > 0 {
		cp.Price = *p.Price
	}
	if p.CategoryIDs != nil {
		cp.CategoryIDs = *p.CategoryIDs
	}
	if p.Lat != nil {
		cp.Lat = *p.Lat
	}
	if p.Lng != nil {
		cp.Lng = *p.Lng
	}
	if p.AvatarURL != nil {
		cp.AvatarURL = *p.AvatarURL
	}
	if p.Address != nil {
		cp.Address = *p.Address
	}
	if p.WorkingHours != nil {
		cp.WorkingHours = *p.WorkingHours
	}
	if p.EmployeesCount != nil && *p.EmployeesCount >= 0 {
		cp.EmployeesCount = *p.EmployeesCount
	}
	if p.ExperienceYears != nil && *p.ExperienceYears >= 0 {
		cp.ExperienceYears = *p.ExperienceYears
	}
	if p.Email != nil {
		cp.Email = *p.Email
	}
	if p.Website != nil {
		cp.Website = *p.Website
	}
	if cp.Images == nil {
		cp.Images = []string{}
	}
	if cp.CategoryIDs == nil {
		cp.CategoryIDs = []string{}
	}
}

// validCategories reports whether every id in a patch names a real category.
// Nil (field not sent) passes — only what was written is checked.
func (s *Store) validCategories(ids *[]string) bool {
	if ids == nil {
		return true
	}
	for _, c := range *ids {
		if !s.categoryExists(c) {
			return false
		}
	}
	return true
}

// companyUpdateSQL writes every editable column except phone, which only the
// admin path sets. Callers append their own WHERE clause starting at $15.
const companyUpdateSQL = `UPDATE companies SET name=$1, description=$2, images=$3, price=$4,
	avatar_url=$5, address=$6, working_hours=$7, employees_count=$8, experience_years=$9,
	email=$10, website=$11, category_ids=$12, lat=$13, lng=$14`

// companyUpdateArgs is the $1..$14 tail every writer of companyUpdateSQL passes.
func companyUpdateArgs(cp *models.CompanyProfile) []any {
	return []any{
		cp.Name, cp.Description, marshalJSON(cp.Images), cp.Price, cp.AvatarURL, cp.Address,
		cp.WorkingHours, cp.EmployeesCount, cp.ExperienceYears, cp.Email, cp.Website,
		marshalJSON(cp.CategoryIDs), cp.Lat, cp.Lng,
	}
}

func (s *Store) UpdateCompanyProfile(userID string, p CompanyPatch) (*models.CompanyProfile, error) {
	cp, ok := s.getCompanyByUser(userID)
	if !ok {
		return nil, ErrNotFound
	}
	if !s.validCategories(p.CategoryIDs) {
		return nil, ErrBadRequest
	}
	p.apply(cp)
	s.db.Exec(companyUpdateSQL+" WHERE user_id=$15", append(companyUpdateArgs(cp), userID)...)
	return cp, nil
}

func (s *Store) SetCompanyStatus(userID string, online bool) (*models.CompanyProfile, error) {
	cp, ok := s.getCompanyByUser(userID)
	if !ok {
		return nil, ErrNotFound
	}
	cp.Online = online
	s.db.Exec("UPDATE companies SET online=$1 WHERE user_id=$2", online, userID)
	return cp, nil
}

// ListOnlineCompanies returns bookable companies: online, not banned, not
// already on a job, and never the caller's own company.
func (s *Store) ListOnlineCompanies(excludeUserID string) []models.CompanyProfile {
	rows, err := s.db.Query("SELECT " + companyCols + " FROM companies WHERE online=TRUE AND banned=FALSE")
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []models.CompanyProfile
	for rows.Next() {
		cp, err := s.scanCompany(rows)
		if err != nil {
			continue
		}
		if cp.UserID == excludeUserID || s.CompanyIsBusy(cp.ID) {
			continue
		}
		out = append(out, *cp)
	}
	return out
}

// NearbyCompanies is ListOnlineCompanies narrowed to firms within radiusKm of a
// point, nearest first.
//
// A firm with no pin on its record (0,0 — every firm created before the map was
// part of the form) is kept and listed after the located ones. Dropping it would
// make the office's own back catalogue vanish from the customer's list the day
// this shipped, which is a worse failure than an unsorted row at the bottom.
func (s *Store) NearbyCompanies(lat, lng, radiusKm float64, excludeUserID string) []models.CompanyProfile {
	all := s.ListOnlineCompanies(excludeUserID)
	if radiusKm <= 0 || (lat == 0 && lng == 0) {
		return all
	}
	out := []models.CompanyProfile{}
	dist := map[string]float64{}
	for _, cp := range all {
		if cp.Lat == 0 && cp.Lng == 0 {
			dist[cp.ID] = math.MaxFloat64
			out = append(out, cp)
			continue
		}
		d := geo.DistanceKm(lat, lng, cp.Lat, cp.Lng)
		if d > radiusKm {
			continue
		}
		dist[cp.ID] = d
		km := d
		cp.DistanceKm = &km
		out = append(out, cp)
	}
	sort.Slice(out, func(i, j int) bool { return dist[out[i].ID] < dist[out[j].ID] })
	return out
}

func (s *Store) GetCompanyByID(companyID string) (*models.CompanyProfile, bool) {
	cp, err := s.scanCompany(s.db.QueryRow("SELECT "+companyCols+" FROM companies WHERE id=$1", companyID))
	if err != nil {
		return nil, false
	}
	return cp, true
}

func (s *Store) getCompanyByUser(userID string) (*models.CompanyProfile, bool) {
	cp, err := s.scanCompany(s.db.QueryRow("SELECT "+companyCols+" FROM companies WHERE user_id=$1", userID))
	if err != nil {
		return nil, false
	}
	return cp, true
}

// Admin company methods.

func (s *Store) ListAllCompanies() []models.CompanyProfile {
	rows, err := s.db.Query("SELECT " + companyCols + " FROM companies ORDER BY created_at DESC")
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []models.CompanyProfile
	for rows.Next() {
		cp, err := s.scanCompany(rows)
		if err == nil {
			out = append(out, *cp)
		}
	}
	return out
}

func (s *Store) BanCompany(companyID string) error {
	res, _ := s.db.Exec("UPDATE companies SET banned=TRUE, online=FALSE WHERE id=$1", companyID)
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) UnbanCompany(companyID string) error {
	res, _ := s.db.Exec("UPDATE companies SET banned=FALSE WHERE id=$1", companyID)
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) AdminUpdateCompany(companyID string, p CompanyPatch) (*models.CompanyProfile, error) {
	cp, ok := s.GetCompanyByID(companyID)
	if !ok {
		return nil, ErrNotFound
	}
	if !s.validCategories(p.CategoryIDs) {
		return nil, ErrBadRequest
	}
	p.apply(cp)
	if p.Phone != nil {
		cp.Phone = normalizePhone(*p.Phone)
	}
	s.db.Exec(companyUpdateSQL+", phone=$15 WHERE id=$16",
		append(companyUpdateArgs(cp), cp.Phone, companyID)...)
	return cp, nil
}
