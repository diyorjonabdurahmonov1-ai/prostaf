package store

import (
	"strings"
	"unicode"

	"prostaff/internal/id"
	"prostaff/internal/models"
)

// The service list the whole platform is indexed by.
//
// It used to be a fixture compiled into the binary: seven rows, seeded once, and
// changing one meant a deploy. It is the office's list now — they are the people
// who find out that nobody searches for "Ko'k tushuruvchi" and that half the
// calls this month were for air conditioners.
//
// Two rules make that safe to hand over:
//
//   - A category is retired, not deleted, once anything points at it. An order
//     taken last year still carries its category id and still has to be able to
//     show a name; deleting the row would blank the label on a record nobody can
//     re-enter. Retiring keeps the name and takes it off every picker, which is
//     what "we don't do that any more" actually means.
//   - A category nothing points at can be deleted outright. That is the typo
//     the operator made two minutes ago, and refusing to remove it would leave
//     the list permanently dirtier than the day it was handed over.

const categoryCols = "id, key, name_uz, name_ru, name_en, icon, active, sort"

func scanCategory(row interface{ Scan(dest ...any) error }) (*models.Category, error) {
	var c models.Category
	if err := row.Scan(&c.ID, &c.Key, &c.NameUz, &c.NameRu, &c.NameEn, &c.Icon,
		&c.Active, &c.Sort); err != nil {
		return nil, err
	}
	return &c, nil
}

// Categories returns the whole list, retired rows included.
//
// Everything is returned because the apps use this list for two different jobs:
// filling a picker, where only the live rows belong, and turning the category id
// on an existing order into a name, where a retired row is exactly the one being
// asked about. Filtering here would answer the first question by breaking the
// second, so the flag travels with the row and the picker does the filtering.
func (s *Store) Categories() []models.Category {
	rows, err := s.db.Query("SELECT " + categoryCols + " FROM categories ORDER BY sort, name_uz")
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := []models.Category{}
	for rows.Next() {
		if c, err := scanCategory(rows); err == nil {
			out = append(out, *c)
		}
	}
	return out
}

// CategoriesWithUsage is the admin's view: the same list, plus what each row is
// holding. The counts are what the panel needs to tell the office whether a
// category can be deleted or only retired, before they tap the button.
func (s *Store) CategoriesWithUsage() []models.Category {
	out := s.Categories()
	for i := range out {
		orders, providers := s.categoryUsage(out[i].ID)
		out[i].OrdersCount = orders
		out[i].ProvidersCount = providers
	}
	return out
}

// GetCategory returns one row.
func (s *Store) GetCategory(catID string) (*models.Category, bool) {
	c, err := scanCategory(s.db.QueryRow("SELECT "+categoryCols+" FROM categories WHERE id=$1", catID))
	if err != nil {
		return nil, false
	}
	return c, true
}

func (s *Store) categoryExists(catID string) bool {
	var n int
	s.db.QueryRow("SELECT COUNT(*) FROM categories WHERE id=$1", catID).Scan(&n)
	return n > 0
}

// categoryBookable reports whether a new order may name this category. Retired
// rows fail: they stay readable on the records that already reference them, but
// nothing new is filed under them.
func (s *Store) categoryBookable(catID string) bool {
	var active bool
	if s.db.QueryRow("SELECT active FROM categories WHERE id=$1", catID).Scan(&active) != nil {
		return false
	}
	return active
}

// categoryUsage counts the orders filed under a category and the providers who
// list it. The provider count spans workers and companies — both store their
// categories as a JSON array of ids, so it is a substring match on the quoted
// id rather than a join.
func (s *Store) categoryUsage(catID string) (orders, providers int) {
	s.db.QueryRow("SELECT COUNT(*) FROM orders WHERE category_id=$1", catID).Scan(&orders)
	needle := "%\"" + catID + "\"%"
	var workers, companies int
	s.db.QueryRow("SELECT COUNT(*) FROM workers WHERE category_ids LIKE $1", needle).Scan(&workers)
	s.db.QueryRow("SELECT COUNT(*) FROM companies WHERE category_ids LIKE $1", needle).Scan(&companies)
	return orders, workers + companies
}

// CategoryInput is a category as the office describes it. Only the Uzbek name is
// required — it is the one every screen falls back to.
type CategoryInput struct {
	Key    string
	NameUz string
	NameRu string
	NameEn string
	Icon   string
	Active *bool
	Sort   *int
}

// CreateCategory adds a service to the list.
func (s *Store) CreateCategory(in CategoryInput) (*models.Category, error) {
	in.NameUz = strings.TrimSpace(in.NameUz)
	if in.NameUz == "" {
		return nil, ErrBadRequest
	}
	// The other two languages fall back to the Uzbek name rather than to blank:
	// a Russian-speaking customer reading an empty chip is worse off than one
	// reading a name in the wrong language.
	in.NameRu = firstNonEmpty(strings.TrimSpace(in.NameRu), in.NameUz)
	in.NameEn = firstNonEmpty(strings.TrimSpace(in.NameEn), in.NameUz)

	key := slugify(firstNonEmpty(strings.TrimSpace(in.Key), in.NameUz))
	if key == "" {
		// A name with nothing latinisable in it — Cyrillic, emoji — still needs a
		// key, and the id is unique by construction.
		key = "cat_" + id.New()[:8]
	}
	if s.categoryKeyTaken(key, "") {
		return nil, ErrDuplicateCategory
	}

	active := true
	if in.Active != nil {
		active = *in.Active
	}
	sort := 0
	if in.Sort != nil {
		sort = *in.Sort
	} else {
		// New rows land at the end of the list rather than on top of whatever is
		// currently first — the office decides the order, not the clock.
		s.db.QueryRow("SELECT COALESCE(MAX(sort), 0) + 1 FROM categories").Scan(&sort)
	}

	catID := id.New()
	if _, err := s.db.Exec(`INSERT INTO categories (id, key, name_uz, name_ru, name_en, icon, active, sort)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		catID, key, in.NameUz, in.NameRu, in.NameEn, strings.TrimSpace(in.Icon), active, sort); err != nil {
		return nil, ErrDuplicateCategory
	}
	c, _ := s.GetCategory(catID)
	return c, nil
}

// CategoryPatch is a correction. Everything is optional; only what was sent is
// written.
type CategoryPatch struct {
	Key    *string
	NameUz *string
	NameRu *string
	NameEn *string
	Icon   *string
	Active *bool
	Sort   *int
}

// UpdateCategory applies an edit.
//
// Renaming is safe at any time and is the point of the screen: the id never
// changes, so every order and every provider that pointed at this row keeps
// pointing at it and simply starts reading the new name.
func (s *Store) UpdateCategory(catID string, p CategoryPatch) (*models.Category, error) {
	c, ok := s.GetCategory(catID)
	if !ok {
		return nil, ErrNotFound
	}
	if p.NameUz != nil {
		name := strings.TrimSpace(*p.NameUz)
		if name == "" {
			return nil, ErrBadRequest
		}
		c.NameUz = name
	}
	if p.NameRu != nil {
		c.NameRu = firstNonEmpty(strings.TrimSpace(*p.NameRu), c.NameUz)
	}
	if p.NameEn != nil {
		c.NameEn = firstNonEmpty(strings.TrimSpace(*p.NameEn), c.NameUz)
	}
	if p.Icon != nil {
		c.Icon = strings.TrimSpace(*p.Icon)
	}
	if p.Key != nil {
		key := slugify(*p.Key)
		if key != "" && key != c.Key {
			if s.categoryKeyTaken(key, catID) {
				return nil, ErrDuplicateCategory
			}
			c.Key = key
		}
	}
	if p.Active != nil {
		c.Active = *p.Active
	}
	if p.Sort != nil {
		c.Sort = *p.Sort
	}
	if _, err := s.db.Exec(`UPDATE categories
		SET key=$1, name_uz=$2, name_ru=$3, name_en=$4, icon=$5, active=$6, sort=$7 WHERE id=$8`,
		c.Key, c.NameUz, c.NameRu, c.NameEn, c.Icon, c.Active, c.Sort, catID); err != nil {
		return nil, ErrDuplicateCategory
	}
	return c, nil
}

// DeleteCategory removes a category that nothing points at.
//
// Anything else is refused with ErrCategoryInUse, which the panel turns into the
// one sentence worth saying: retire it instead. Deleting a category an order
// references would leave that order with a blank service on every screen that
// shows it, and there is no way back — the customer who placed it is not going
// to be asked what it was for.
func (s *Store) DeleteCategory(catID string) error {
	if _, ok := s.GetCategory(catID); !ok {
		return ErrNotFound
	}
	if orders, providers := s.categoryUsage(catID); orders > 0 || providers > 0 {
		return ErrCategoryInUse
	}
	res, err := s.db.Exec("DELETE FROM categories WHERE id=$1", catID)
	if err != nil {
		return ErrConflict
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) categoryKeyTaken(key, exceptID string) bool {
	var n int
	s.db.QueryRow("SELECT COUNT(*) FROM categories WHERE key=$1 AND id <> $2", key, exceptID).Scan(&n)
	return n > 0
}

func firstNonEmpty(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// slugify turns a typed name into the stable key the seeds use ("Bog' ishlari"
// → "bog_ishlari"). Only ASCII letters and digits survive; everything else
// becomes a separator, and runs of separators collapse.
func slugify(v string) string {
	var b strings.Builder
	lastUnderscore := true // leading separators are dropped
	for _, r := range strings.ToLower(strings.TrimSpace(v)) {
		switch {
		case r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			b.WriteRune(r)
			lastUnderscore = false
		case !lastUnderscore:
			b.WriteByte('_')
			lastUnderscore = true
		}
	}
	return strings.Trim(b.String(), "_")
}
