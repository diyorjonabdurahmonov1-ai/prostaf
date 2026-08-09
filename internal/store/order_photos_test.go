package store

import (
	"testing"

	"prostaff/internal/models"
)

// The compatibility seam between an app that sends one photo and one that sends
// a list. Get it wrong and either the old app shows no picture on a new order,
// or the new one drops every photo but the first.
func TestNormalizeOrderPhotos(t *testing.T) {
	cases := []struct {
		name      string
		in        models.Order
		wantFirst string
		wantCount int
	}{
		{
			name:      "a new app sends a list",
			in:        models.Order{Images: []string{"a", "b", "c"}},
			wantFirst: "a",
			wantCount: 3,
		},
		{
			// The old app's single field has to become the list, or a worker on
			// a new build sees an order with no photos at all.
			name:      "an old app sends one url",
			in:        models.Order{ImageURL: "a"},
			wantFirst: "a",
			wantCount: 1,
		},
		{
			// And the reverse: an old app reads image_url and nothing else, so
			// it must never be empty while there are photos.
			name:      "the list fills the single field",
			in:        models.Order{Images: []string{"x", "y"}},
			wantFirst: "x",
			wantCount: 2,
		},
		{
			name:      "no photos at all",
			in:        models.Order{},
			wantFirst: "",
			wantCount: 0,
		},
		{
			name: "past the cap the extras are dropped",
			in: models.Order{
				Images: []string{"1", "2", "3", "4", "5", "6", "7", "8"},
			},
			wantFirst: "1",
			wantCount: maxOrderPhotos,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := tc.in
			normalizeOrderPhotos(&o)
			if o.ImageURL != tc.wantFirst {
				t.Errorf("ImageURL = %q, want %q", o.ImageURL, tc.wantFirst)
			}
			if len(o.Images) != tc.wantCount {
				t.Errorf("len(Images) = %d, want %d", len(o.Images), tc.wantCount)
			}
			// Never nil: the field is serialised into JSON that clients index
			// into, and `null` there is a crash rather than an empty gallery.
			if o.Images == nil {
				t.Error("Images must be an empty list, never nil")
			}
			if len(o.Images) > 0 && o.Images[0] != o.ImageURL {
				t.Errorf("image_url %q must be the head of the list %v", o.ImageURL, o.Images)
			}
		})
	}
}
