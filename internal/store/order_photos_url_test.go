package store

import "testing"

// The one piece of parsing between an order's photo list and the rows that get
// deleted. Too loose and it hands the delete a string that matches some other
// file; too strict and a photo is never tagged, so it is kept forever and the
// disk fills anyway — which is the bug this whole feature exists to fix.
func TestFileIDFromURL(t *testing.T) {
	const validID = "f47ac10b-58cc-4372-a567-0e02b2c3d479"

	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "what the upload endpoint hands back",
			in:   "https://api.prostaff.uz/api/v1/files/" + validID,
			want: validID,
		},
		{
			// Development, and the fallback base URL the app ships with.
			name: "plain http and a port",
			in:   "http://192.168.1.10:8000/api/v1/files/" + validID,
			want: validID,
		},
		{
			// A client appending one is not wrong, and losing the photo over it
			// would be.
			name: "a cache buster",
			in:   "https://api.prostaff.uz/api/v1/files/" + validID + "?v=2",
			want: validID,
		},
		{
			name: "a fragment",
			in:   "https://api.prostaff.uz/api/v1/files/" + validID + "#top",
			want: validID,
		},
		{
			name: "a trailing slash",
			in:   "https://api.prostaff.uz/api/v1/files/" + validID + "/",
			want: validID,
		},
		{
			name: "surrounding whitespace",
			in:   "  https://api.prostaff.uz/api/v1/files/" + validID + "  ",
			want: validID,
		},
		{
			name: "the bare id on its own",
			in:   validID,
			want: validID,
		},
		{
			// Seeded and demo profiles point at images that are not ours. Nothing
			// should be derived from them.
			name: "somebody else's url",
			in:   "https://images.example.com/photo.jpg",
			want: "",
		},
		{
			// The shape check is what stops a path segment being sent to the
			// database as a candidate file id.
			name: "a path that is not an id",
			in:   "https://api.prostaff.uz/api/v1/files/latest",
			want: "",
		},
		{
			name: "too short to be an id",
			in:   "https://api.prostaff.uz/api/v1/files/f47ac10b",
			want: "",
		},
		{name: "empty", in: "", want: ""},
		{name: "whitespace only", in: "   ", want: ""},
		{name: "a slash only", in: "/", want: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := fileIDFromURL(tc.in); got != tc.want {
				t.Errorf("fileIDFromURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// The statuses a photo may be deleted for. A status that leaked in here by
// accident would delete the photos of a job that is still being done — the
// worker on their way to it opens the order and finds the pictures gone.
func TestTerminalOrderStatuses(t *testing.T) {
	live := []string{"pending", "matching", "invited", "accepted", "in_progress"}
	for _, s := range terminalOrderStatuses {
		for _, l := range live {
			if s == l {
				t.Errorf("%q is a live order status and must not be purgeable", s)
			}
		}
	}
	if len(terminalOrderStatuses) != 3 {
		t.Errorf("expected completed/cancelled/expired, got %v", terminalOrderStatuses)
	}
}

// pgTextArray builds the literal that `= ANY($1)` is given, because database/sql
// has no array parameter to pass instead.
func TestPGTextArray(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want string
	}{
		{
			name: "the statuses it is actually called with",
			in:   []string{"completed", "cancelled", "expired"},
			want: `{"completed","cancelled","expired"}`,
		},
		{name: "one value", in: []string{"expired"}, want: `{"expired"}`},
		{name: "none", in: nil, want: `{}`},
		{
			// Not reachable from the current callers, and the whole point of
			// checking it: a helper is only safe until somebody reuses it.
			name: "a quote is escaped rather than closing the value",
			in:   []string{`a"b`},
			want: `{"a\"b"}`,
		},
		{
			name: "a backslash is escaped",
			in:   []string{`a\b`},
			want: `{"a\\b"}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pgTextArray(tc.in); got != tc.want {
				t.Errorf("pgTextArray(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
