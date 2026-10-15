package store

import (
	"database/sql"
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
	// PassportSeries is required: it is the worker's identity on the platform and
	// the key their free trial jobs are counted under.
	PassportSeries string
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
		&wp.AvatarURL, &wp.About, &wp.ExperienceYears, &wp.Address, &images,
		&wp.PassportSeries, &wp.FreeOrdersUsed)
	if err != nil {
		return nil, err
	}
	wp.CategoryIDs = unmarshalStrings(catIDs)
	wp.Images = unmarshalStrings(images)
	// The limit is a setting, not a column: raise it and every worker's remaining
	// trial jobs go up with it, including the ones already signed up.
	wp.FreeOrdersLimit = s.FreeOrderLimit()
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
//
// The trial counter is read the same way — one correlated subquery instead of a
// second round trip per worker, which matters on the dispatch path where every
// online worker in the city is scanned for every order.
const workerCols = `id, user_id, full_name, phone, gender, age, price_min, price_max, lat, lng,
	category_ids, online, banned, credit_limit, rating, ratings_count, created_at,
	COALESCE(NULLIF(avatar_url, ''), (SELECT u.avatar_url FROM users u WHERE u.id = workers.user_id), '') AS avatar_url,
	about, experience_years, address, images, passport_series,
	COALESCE((SELECT ft.used FROM free_trials ft
		WHERE workers.passport_series <> '' AND ft.key = 'worker:' || workers.passport_series), 0) AS free_orders_used`

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
	// The passport is checked before anything is written. A worker whose series
	// is missing or malformed would carry no trial ledger key at all, which
	// silently costs them their three free jobs — better to refuse the form.
	passport := NormalizePassport(in.PassportSeries)
	if !ValidPassport(passport) {
		return nil, ErrInvalidPassport
	}
	if s.passportTaken(passport, "") {
		return nil, ErrDuplicatePassport
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
		PassportSeries:  passport,
		FreeOrdersUsed:  s.freeOrdersUsed(WorkerTrialKey(passport)),
		FreeOrdersLimit: s.FreeOrderLimit(),
		CreatedAt:       time.Now(),
	}
	if wp.Images == nil {
		wp.Images = []string{}
	}
	// The insert is checked here, unlike the other writes in this file: the
	// passport carries a unique index, and two operators filling in the same
	// worker at the same time both pass the check above. Losing that race has to
	// be an error the office sees rather than a profile that never appeared.
	if _, err := s.db.Exec(`INSERT INTO workers (id, user_id, full_name, phone, gender, age, price_min, price_max,
		lat, lng, category_ids, created_at, avatar_url, about, experience_years, address, images, passport_series)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`,
		wp.ID, wp.UserID, wp.FullName, wp.Phone, wp.Gender, wp.Age, wp.PriceMin, wp.PriceMax, wp.Lat, wp.Lng,
		marshalJSON(wp.CategoryIDs), wp.CreatedAt,
		wp.AvatarURL, wp.About, wp.ExperienceYears, wp.Address, marshalJSON(wp.Images),
		wp.PassportSeries); err != nil {
		return nil, ErrDuplicatePassport
	}
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
//
// The order of the two filters is the whole cost of this function. Distance is
// free and decides most of it; "already on a job" costs a lookup. Asking the
// database about every online worker on the platform and then dropping most of
// the answers on the radius is what this did, on a screen customers open far
// more often than anyone places an order — so the radius runs first and the one
// lookup left is asked once, for the workers that are actually nearby.
func (s *Store) NearbyWorkers(lat, lng, radiusKm float64, excludeUserID string) []models.WorkerProfile {
	if radiusKm <= 0 {
		radiusKm = matchRadiusKm
	}
	rows, err := s.db.Query("SELECT " + workerCols + " FROM workers WHERE online=TRUE AND banned=FALSE")
	if err != nil {
		return []models.WorkerProfile{}
	}

	near := []models.WorkerProfile{}
	for rows.Next() {
		wp, err := s.scanWorker(rows)
		if err != nil {
			continue
		}
		if wp.UserID == excludeUserID {
			continue
		}
		d := geo.DistanceKm(lat, lng, wp.Lat, wp.Lng)
		if d > radiusKm {
			continue
		}
		dist := d
		wp.DistanceKm = &dist
		near = append(near, *wp)
	}
	// Closed before the sweep below rather than deferred: a cursor holds a pool
	// connection for as long as it is open, and querying inside one takes a
	// second. See shortlistFor.
	rows.Close()
	if len(near) == 0 {
		return near
	}

	userIDs := make([]string, len(near))
	for i := range near {
		userIDs[i] = near[i].UserID
	}
	busy := s.busyWorkers(userIDs)

	out := make([]models.WorkerProfile, 0, len(near))
	for _, wp := range near {
		if busy[wp.UserID] {
			continue
		}
		out = append(out, wp)
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
	// Services is what the firm does, written out. Optional.
	Services string
	Lat      float64
	Lng      float64
	// INN is required, for the same reason a worker's passport is: it is the firm
	// on paper, and the key its free trial jobs are counted under.
	INN string

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
	var until, liveAt sql.NullTime
	err := row.Scan(&cp.ID, &cp.UserID, &cp.Name, &cp.Phone, &cp.Description, &images,
		&cp.Online, &cp.Banned, &cp.Rating, &cp.RatingsCount, &cp.CreatedAt, &cp.Price,
		&cp.AvatarURL, &cp.Address, &cp.WorkingHours, &cp.EmployeesCount, &cp.ExperienceYears,
		&cp.Email, &cp.Website, &categoryIDs, &cp.Services, &cp.Lat, &cp.Lng,
		&cp.INN, &cp.FreeOrdersUsed, &until,
		&cp.LiveLocation, &cp.LiveLat, &cp.LiveLng, &liveAt)
	if err != nil {
		return nil, err
	}
	if liveAt.Valid {
		at := liveAt.Time
		cp.LiveUpdatedAt = &at
	}
	cp.Images = unmarshalStrings(images)
	cp.CategoryIDs = unmarshalStrings(categoryIDs)
	cp.FreeOrdersLimit = s.FreeOrderLimit()
	if until.Valid {
		end := until.Time
		cp.SubscriptionUntil = &end
		cp.SubscriptionActive = end.After(time.Now())
		if cp.SubscriptionActive {
			// Rounded up, so the last partial day still reads as a day left.
			cp.SubscriptionDaysLeft = int(time.Until(end).Hours()/24) + 1
		}
	}
	return &cp, nil
}

const companyCols = `id, user_id, name, phone, description, images, online, banned, rating,
	ratings_count, created_at, price, avatar_url, address, working_hours, employees_count,
	experience_years, email, website, category_ids, services, lat, lng, inn,
	COALESCE((SELECT ft.used FROM free_trials ft
		WHERE companies.inn <> '' AND ft.key = 'company:' || companies.inn), 0) AS free_orders_used,
	subscription_until, live_location, live_lat, live_lng, live_updated_at`

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
	inn := NormalizeINN(in.INN)
	if !ValidINN(inn) {
		return nil, ErrInvalidINN
	}
	if s.innTaken(inn, "") {
		return nil, ErrDuplicateINN
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
		Services:        in.Services,
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
		INN:             inn,
		FreeOrdersUsed:  s.freeOrdersUsed(CompanyTrialKey(inn)),
		FreeOrdersLimit: s.FreeOrderLimit(),
		CreatedAt:       time.Now(),
	}
	if cp.Images == nil {
		cp.Images = []string{}
	}
	if cp.CategoryIDs == nil {
		cp.CategoryIDs = []string{}
	}
	// Checked for the same reason the worker insert is: the INN is unique, and
	// two operators typing in the same firm at once both get past the check above.
	if _, err := s.db.Exec(`INSERT INTO companies (id, user_id, name, phone, description, images, created_at, price,
		avatar_url, address, working_hours, employees_count, experience_years, email, website,
		category_ids, services, lat, lng, inn)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)`,
		cp.ID, cp.UserID, cp.Name, cp.Phone, cp.Description,
		marshalJSON(cp.Images), cp.CreatedAt, cp.Price,
		cp.AvatarURL, cp.Address, cp.WorkingHours, cp.EmployeesCount, cp.ExperienceYears,
		cp.Email, cp.Website, marshalJSON(cp.CategoryIDs), cp.Services, cp.Lat, cp.Lng, cp.INN); err != nil {
		return nil, ErrDuplicateINN
	}
	s.addRoleInternal(userID, models.RoleCompany)
	return cp, nil
}

type CompanyPatch struct {
	Name        *string
	Description *string
	Images      *[]string
	Price       *int64
	CategoryIDs *[]string
	Services    *string
	Lat         *float64
	Lng         *float64
	// Phone and INN are admin-only: the company's contact number and its tax
	// number are part of the record the office fills in, not something the
	// account edits itself.
	Phone *string
	INN   *string

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
	// Blank is a real value here, not a missing one: clearing the line is how a
	// firm takes it off its page.
	if p.Services != nil {
		cp.Services = *p.Services
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
// admin path sets. Callers append their own WHERE clause starting at $16.
const companyUpdateSQL = `UPDATE companies SET name=$1, description=$2, images=$3, price=$4,
	avatar_url=$5, address=$6, working_hours=$7, employees_count=$8, experience_years=$9,
	email=$10, website=$11, category_ids=$12, services=$13, lat=$14, lng=$15`

// companyUpdateArgs is the $1..$15 tail every writer of companyUpdateSQL passes.
func companyUpdateArgs(cp *models.CompanyProfile) []any {
	return []any{
		cp.Name, cp.Description, marshalJSON(cp.Images), cp.Price, cp.AvatarURL, cp.Address,
		cp.WorkingHours, cp.EmployeesCount, cp.ExperienceYears, cp.Email, cp.Website,
		marshalJSON(cp.CategoryIDs), cp.Services, cp.Lat, cp.Lng,
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
	s.db.Exec(companyUpdateSQL+" WHERE user_id=$16", append(companyUpdateArgs(cp), userID)...)
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

// SetCompanyLiveLocation switches a firm's live location and records where its
// phone is now. One call does both jobs — turning it on, and reporting moves
// while it is on — so a point that arrives with enabled=true is always stored.
//
// Turning it on needs a point to start from: a firm switched "live" with
// nowhere to be found would be matched against 0,0. Turning it off forgets the
// point, so a position the firm stopped sharing is not left on record.
func (s *Store) SetCompanyLiveLocation(userID string, enabled bool, lat, lng *float64) (*models.CompanyProfile, error) {
	cp, ok := s.getCompanyByUser(userID)
	if !ok {
		return nil, ErrNotFound
	}
	if !enabled {
		s.db.Exec(`UPDATE companies SET live_location=FALSE, live_lat=0, live_lng=0, live_updated_at=NULL
			WHERE user_id=$1`, userID)
		cp.LiveLocation, cp.LiveLat, cp.LiveLng, cp.LiveUpdatedAt = false, 0, 0, nil
		return cp, nil
	}
	hasPoint := lat != nil && lng != nil && (*lat != 0 || *lng != 0)
	if hasPoint && (*lat < -90 || *lat > 90 || *lng < -180 || *lng > 180) {
		return nil, ErrBadRequest
	}
	if !hasPoint {
		// Already on, and the app only re-asserted it: keep the last point.
		if cp.LiveLocation && (cp.LiveLat != 0 || cp.LiveLng != 0) {
			return cp, nil
		}
		return nil, ErrBadRequest
	}
	now := time.Now()
	s.db.Exec(`UPDATE companies SET live_location=TRUE, live_lat=$1, live_lng=$2, live_updated_at=$3
		WHERE user_id=$4`, *lat, *lng, now, userID)
	cp.LiveLocation, cp.LiveLat, cp.LiveLng, cp.LiveUpdatedAt = true, *lat, *lng, &now
	return cp, nil
}

// hasLivePoint reports whether a firm is sharing a usable live location.
func hasLivePoint(cp *models.CompanyProfile) bool {
	return cp.LiveLocation && (cp.LiveLat != 0 || cp.LiveLng != 0)
}

// ListOnlineCompanies returns bookable companies: online, not banned, and never
// the caller's own company. A firm on a job stays listed — it is a team.
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
		if cp.UserID == excludeUserID {
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
// part of the form) is dropped from a located search. It used to be kept and
// listed after the located ones, so that the office's back catalogue did not
// vanish the day the map shipped; the cost of that was a "nearby" list which
// was not nearby. Every unpinned firm on the platform showed up to every
// customer at every radius, which is how a test firm in another city ended up
// in the results — and a customer cannot tell a stale record from a firm that
// is genuinely around the corner.
//
// The fix for such a firm is to give it a pin (admin → company → location), not
// to show it to everybody. Without a point to measure from — the caller sent no
// coordinates, or no radius — nothing is filtered and the whole online list
// comes back as before.
func (s *Store) NearbyCompanies(lat, lng, radiusKm float64, excludeUserID string) []models.CompanyProfile {
	all := s.ListOnlineCompanies(excludeUserID)
	if radiusKm <= 0 || (lat == 0 && lng == 0) {
		return all
	}
	out := []models.CompanyProfile{}
	dist := map[string]float64{}
	for _, cp := range all {
		// Measured from the office and, for a firm sharing its live location,
		// from where its phone is now — whichever is nearer. A firm with no
		// office pin can still be found by its live point.
		d := -1.0
		if cp.Lat != 0 || cp.Lng != 0 {
			d = geo.DistanceKm(lat, lng, cp.Lat, cp.Lng)
		}
		if hasLivePoint(&cp) {
			if live := geo.DistanceKm(lat, lng, cp.LiveLat, cp.LiveLng); d < 0 || live < d {
				d = live
			}
		}
		if d < 0 || d > radiusKm {
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

// CompanyByUser is the exported reader for a firm's own profile — the API layer
// needs it to answer questions about the caller's billing state.
func (s *Store) CompanyByUser(userID string) (*models.CompanyProfile, bool) {
	return s.getCompanyByUser(userID)
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
	if p.INN != nil {
		inn := NormalizeINN(*p.INN)
		if !ValidINN(inn) {
			return nil, ErrInvalidINN
		}
		if s.innTaken(inn, cp.UserID) {
			return nil, ErrDuplicateINN
		}
		// Its own statement, like a worker's passport, so the firm's trial counter
		// follows the corrected number.
		if _, err := s.db.Exec("UPDATE companies SET inn=$1 WHERE id=$2", inn, cp.ID); err != nil {
			return nil, ErrDuplicateINN
		}
		cp.INN = inn
	}
	p.apply(cp)
	if p.Phone != nil {
		cp.Phone = normalizePhone(*p.Phone)
	}
	s.db.Exec(companyUpdateSQL+", phone=$16 WHERE id=$17",
		append(companyUpdateArgs(cp), cp.Phone, cp.ID)...)
	return cp, nil
}
