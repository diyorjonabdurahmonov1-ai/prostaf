package store

import "testing"

// The key a typed name turns into.
//
// It matters because it is the uniqueness rule: two categories may not share a
// key, so "Bog' ishlari" and "bog ishlari" have to collide, and a name with
// nothing latinisable in it has to come out empty so the caller knows to fall
// back to a generated key rather than storing "" for everybody.
func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"Bog' ishlari":               "bog_ishlari",
		"bog ishlari":                "bog_ishlari",
		"  Xona   tozalash  ":        "xona_tozalash",
		"Remontdan keyingi tozalash": "remontdan_keyingi_tozalash",
		"Konditsioner (o'rnatish)":   "konditsioner_o_rnatish",
		"24/7 xizmat":                "24_7_xizmat",
		"already_a_key":              "already_a_key",
		// Nothing ASCII survives, so the caller has to generate a key instead.
		"Уборка": "",
		"🎉":      "",
		"":       "",
	}
	for in, want := range cases {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

// A category typed in one language still has to read as something in the other
// two — a blank chip is worse than a chip in the wrong language.
func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("", "Bog' ishlari"); got != "Bog' ishlari" {
		t.Errorf("firstNonEmpty fell through to %q, want the fallback", got)
	}
	if got := firstNonEmpty("Садовые работы", "Bog' ishlari"); got != "Садовые работы" {
		t.Errorf("firstNonEmpty overrode a real value with %q", got)
	}
}
