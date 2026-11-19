package api

import (
	"strings"
	"testing"
)

func TestLocalizeKeepsUzbekAndTranslatesOthers(t *testing.T) {
	title, body := localize("uz", "Ish tugadi", "asl matn", nil)
	if title != "Ish tugadi" || body != "asl matn" {
		t.Fatalf("uzbek must stay as written: %q %q", title, body)
	}
	title, body = localize("ru", "Ish tugadi", "asl matn", nil)
	if title != "Работа завершена" || body == "asl matn" {
		t.Fatalf("russian expected: %q %q", title, body)
	}
	title, body = localize("en", "Buyurtma qabul qilindi!", "x", []any{"Aziz", "+998901112233"})
	if title != "Order accepted!" || !strings.Contains(body, "Aziz") || !strings.Contains(body, "+998901112233") {
		t.Fatalf("english args not filled: %q %q", title, body)
	}
	// An order's description is the customer's own words: never replaced.
	_, body = localize("ru", "Yangi buyurtma!", "Kranni tuzatish", nil)
	if body != "Kranni tuzatish" {
		t.Fatalf("description must be kept: %q", body)
	}
	// A title with no entry (office-typed) stands as is.
	title, body = localize("en", "Maxsus xabar", "matn", nil)
	if title != "Maxsus xabar" || body != "matn" {
		t.Fatalf("unlisted must stand: %q %q", title, body)
	}
}

func TestEveryTranslationHasBothLanguages(t *testing.T) {
	for uz, m := range notifyI18n {
		for _, lang := range []string{"ru", "en"} {
			if m[lang].title == "" {
				t.Errorf("%q has no %s title", uz, lang)
			}
		}
	}
}
