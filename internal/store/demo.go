package store

import (
	"log"

	"golang.org/x/crypto/bcrypt"
)

/*
Demo providers — a populated platform to develop and test against.

A fresh database has categories, an admin and nothing else, so every screen that
matters (nearby workers, nearby firms, the map, dispatch) opens empty and there
is no way to tell "the list is broken" from "the list is right and there is
nobody there". These rows are that population.

Off unless asked for: `SEED_DEMO=true`. It is deliberately not tied to DEV_MODE,
which defaults on — inventing a dozen bookable workers on somebody's production
database is not a default anybody wants. Idempotent, so leaving the flag set
across restarts is harmless: a phone number that already has a profile is
skipped rather than duplicated.

Every account signs in with the phone below and the password `demo1234`.
*/

// demoPassword is what all seeded accounts sign in with. These are fixtures for
// an empty database, never real people — the flag that creates them is the
// consent to that.
const demoPassword = "demo1234"

type demoWorker struct {
	phone       string
	fullName    string
	gender      string
	age         int
	priceMin    int64
	priceMax    int64
	lat, lng    float64
	categoryKey string
	passport    string
	about       string
	experience  int
	address     string
}

type demoCompany struct {
	phone        string
	name         string
	description  string
	price        int64
	lat, lng     float64
	categoryKey  string
	inn          string
	address      string
	workingHours string
	employees    int
	experience   int
}

// Points are scattered across Tashkent, 2–12 km from the centre (41.3111,
// 69.2797), so a radius filter has something to actually filter: the far ones
// drop out below ~10 km and come back as the slider opens up.
var demoWorkers = []demoWorker{
	{"+998901110001", "Aziz Karimov", "male", 29, 150000, 400000, 41.3155, 69.2490, "master_helper", "AA1000001",
		"10 yildan ortiq tajriba. Santexnika va elektr ishlariga yordam beraman.", 10, "Chilonzor tumani"},
	{"+998901110002", "Bekzod Tursunov", "male", 34, 120000, 300000, 41.2995, 69.3120, "loader",
		"AB1000002", "Og'ir yuklarni ko'tarish va ortish. Har kuni ishlayman.", 7, "Yashnobod tumani"},
	{"+998901110003", "Dilnoza Rasulova", "female", 26, 100000, 250000, 41.3260, 69.2680, "room_clean",
		"AC1000003", "Xonalarni chuqur tozalash, oyna va gilam yuvish.", 4, "Yunusobod tumani"},
	{"+998901110004", "Sardor Nazarov", "male", 41, 200000, 500000, 41.2840, 69.2400, "post_repair_clean",
		"AD1000004", "Remontdan keyingi tozalash bo'yicha brigada boshlig'i.", 15, "Sergeli tumani"},
	{"+998901110005", "Malika Yusupova", "female", 31, 130000, 320000, 41.3410, 69.2870, "banquet_helper",
		"AE1000005", "To'y va bazmlarda ofitsiant va oshxona yordamchisi.", 8, "Mirzo Ulug'bek tumani"},
	{"+998901110006", "Jasur Ergashev", "male", 23, 90000, 220000, 41.3080, 69.2210, "gardening",
		"AF1000006", "Bog' va hovli ishlari, daraxt kesish, o't o'rish.", 3, "Uchtepa tumani"},
	{"+998901110007", "Oybek Sobirov", "male", 37, 160000, 380000, 41.2600, 69.3300, "mover_coal",
		"AG1000007", "Ko'mir va qurilish materiallarini tushirish.", 12, "Bektemir tumani"},
	{"+998901110008", "Nilufar Qodirova", "female", 28, 110000, 260000, 41.3620, 69.3050, "room_clean",
		"AH1000008", "Ofis va kvartira tozalash. Ekologik vositalar bilan.", 5, "Shayxontohur tumani"},
}

var demoCompanies = []demoCompany{
	{"+998902220001", "Toza Uy Servis", "Kvartira, ofis va uylarni professional tozalash xizmati.",
		350000, 41.3190, 69.2560, "room_clean", "300100001", "Chilonzor, 19-kvartal", "09:00 - 20:00", 24, 9},
	{"+998902220002", "Yuk Master", "Ko'chirish, yuk ortish va tushirish. O'z transportimiz bilan.",
		280000, 41.3050, 69.3010, "loader", "300100002", "Yashnobod, Parkent ko'chasi", "08:00 - 22:00", 40, 6},
	{"+998902220003", "Remont Pro", "Remontdan keyingi tozalash va qurilish chiqindilarini chiqarish.",
		600000, 41.2900, 69.2450, "post_repair_clean", "300100003", "Sergeli, 7-mavze", "09:00 - 19:00", 18, 11},
	{"+998902220004", "Bog' Bahor", "Bog'dorchilik, landshaft dizayni va hovli parvarishi.",
		420000, 41.3350, 69.2760, "gardening", "300100004", "Yunusobod, 12-kvartal", "07:00 - 18:00", 12, 14},
	{"+998902220005", "Bazm Servis", "To'y va tadbirlarga ofitsiant, oshpaz va yordamchilar.",
		500000, 41.3480, 69.2950, "banquet_helper", "300100005", "Mirzo Ulug'bek, Buyuk Ipak Yo'li", "10:00 - 23:00", 55, 8},
}

// seedDemoProviders fills an empty platform with bookable workers and firms.
// Called from New when SEED_DEMO is set; a no-op otherwise.
func (s *Store) seedDemoProviders() {
	hash, err := bcrypt.GenerateFromPassword([]byte(demoPassword), bcrypt.DefaultCost)
	if err != nil {
		log.Printf("seed: demo password: %v", err)
		return
	}

	// The category ids are generated at seed time, so the fixtures name the key
	// they were written against and look the id up here. A key that is not in
	// the table (a category the office renamed) leaves the provider uncategorised
	// rather than refusing to create them.
	catID := func(key string) []string {
		var cid string
		if s.db.QueryRow("SELECT id FROM categories WHERE key=$1", key).Scan(&cid) != nil {
			return []string{}
		}
		return []string{cid}
	}

	workers, companies := 0, 0

	for _, d := range demoWorkers {
		phone := normalizePhone(d.phone)
		if u, ok := s.GetUserByPhone(phone); ok {
			if _, exists := s.GetWorkerByUser(u.ID); exists {
				continue
			}
		}
		u, err := s.AdminCreateWorker(AdminWorkerInput{
			Phone:           phone,
			Name:            d.fullName,
			FullName:        d.fullName,
			Gender:          d.gender,
			Age:             d.age,
			PriceMin:        d.priceMin,
			PriceMax:        d.priceMax,
			Lat:             d.lat,
			Lng:             d.lng,
			CategoryIDs:     catID(d.categoryKey),
			PassportSeries:  d.passport,
			About:           d.about,
			ExperienceYears: d.experience,
			Address:         d.address,
			// Online, or the whole point is lost: an offline worker is in no
			// nearby list and gets rung by no order.
			Online: true,
			// Enough headroom to accept jobs without a top-up, so the dispatch
			// path can be exercised end to end.
			CreditLimit: 200000,
		})
		if err != nil {
			log.Printf("seed: demo worker %s: %v", d.fullName, err)
			continue
		}
		s.SetPasswordHash(u.ID, string(hash))
		workers++
	}

	for _, d := range demoCompanies {
		phone := normalizePhone(d.phone)
		if u, ok := s.GetUserByPhone(phone); ok {
			if _, exists := s.getCompanyByUser(u.ID); exists {
				continue
			}
		}
		u, err := s.AdminCreateCompany(AdminCompanyInput{
			Phone:           phone,
			Name:            d.name,
			Description:     d.description,
			Price:           d.price,
			Lat:             d.lat,
			Lng:             d.lng,
			CategoryIDs:     catID(d.categoryKey),
			INN:             d.inn,
			Address:         d.address,
			WorkingHours:    d.workingHours,
			EmployeesCount:  d.employees,
			ExperienceYears: d.experience,
			Online:          true,
			// A firm with no months left takes no orders, and a demo firm that
			// cannot be booked is a demo of the paywall.
			SubscriptionMonths: 12,
		})
		if err != nil {
			log.Printf("seed: demo company %s: %v", d.name, err)
			continue
		}
		s.SetPasswordHash(u.ID, string(hash))
		companies++
	}

	if workers > 0 || companies > 0 {
		log.Printf("seed: %d demo workers, %d demo companies (password %q)", workers, companies, demoPassword)
	}
}
