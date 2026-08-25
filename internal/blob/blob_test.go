package blob

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !s.Enabled() {
		t.Fatal("a store with a directory should be enabled")
	}
	return s
}

func TestUnconfiguredStoreIsDisabled(t *testing.T) {
	// The empty path is how a deployment says "keep the bytes in Postgres". It
	// has to be a quiet no, not an error, or every such deployment fails to boot.
	for _, dir := range []string{"", "   "} {
		s, err := New(dir)
		if err != nil {
			t.Fatalf("New(%q): unexpected error %v", dir, err)
		}
		if s.Enabled() {
			t.Fatalf("New(%q) should be disabled", dir)
		}
		if _, err := s.Put("f47ac10b", "image/jpeg", []byte("x")); err == nil {
			t.Fatal("Put on a disabled store should fail rather than write somewhere")
		}
		// A nil store still has to survive the calls the shutdown path makes.
		if err := s.Remove("a/b/c.jpg"); err != nil {
			t.Fatalf("Remove on a disabled store: %v", err)
		}
		if s.Dir() != "" {
			t.Fatalf("Dir on a disabled store = %q", s.Dir())
		}
	}
}

func TestPutThenOpenRoundTrip(t *testing.T) {
	s := newTestStore(t)
	want := []byte("\xff\xd8\xff\xe0 not really a jpeg")

	rel, err := s.Put("f47ac10b-58cc-4372-a567-0e02b2c3d479", "image/jpeg", want)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Sharded two levels deep so no directory ever holds the whole platform.
	if rel != "f4/7a/f47ac10b-58cc-4372-a567-0e02b2c3d479.jpg" {
		t.Fatalf("path = %q, want it bucketed by the first four hex digits", rel)
	}
	if strings.Contains(rel, "\\") {
		t.Fatalf("path %q must use forward slashes — it is stored in Postgres and "+
			"read back on whatever OS the server runs", rel)
	}

	f, info, err := s.Open(rel)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()
	if info.Size() != int64(len(want)) {
		t.Fatalf("size = %d, want %d", info.Size(), len(want))
	}
	got, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("content = %q, want %q", got, want)
	}
}

func TestPutLeavesNoTempFiles(t *testing.T) {
	// Writes go to a temp file that is renamed into place, so a reader never
	// sees half a photo. The other half of that deal is not littering.
	s := newTestStore(t)
	rel, err := s.Put("aabbccdd-0000-4000-8000-000000000000", "image/png", []byte("png"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(filepath.Join(s.Dir(), filepath.FromSlash(rel))))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("bucket holds %v, want only the finished file", names)
	}
}

func TestPutRejectsWhatTheStoreDoesNotServe(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Put("aabbccdd-0000-4000-8000-000000000000", "application/pdf", []byte("%PDF")); err == nil {
		t.Fatal("a mime type with no suffix should be refused, not saved without one")
	}
	if _, err := s.Put("abc", "image/jpeg", []byte("x")); err == nil {
		t.Fatal("an id too short to shard should be refused")
	}
}

func TestOpenRefusesEscapingPaths(t *testing.T) {
	// Every path Open sees was written by Put into our own database, so this can
	// only fire on a tampered row — which is the case worth refusing, because
	// otherwise a TEXT column decides which file the server hands out.
	s := newTestStore(t)
	secret := filepath.Join(filepath.Dir(s.Dir()), "secret.txt")
	if err := os.WriteFile(secret, []byte("private"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	for _, rel := range []string{
		"../secret.txt",
		"aa/../../secret.txt",
		"..\\secret.txt",
		secret,
		"",
		"   ",
	} {
		if _, _, err := s.Open(rel); err == nil {
			t.Fatalf("Open(%q) succeeded — it must not reach outside %s", rel, s.Dir())
		}
	}
}

func TestRemoveIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	rel, err := s.Put("aabbccdd-0000-4000-8000-000000000000", "image/webp", []byte("webp"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.Remove(rel); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	// Removing again is the cleanup path running twice — the file is gone
	// either way, which is the state the caller asked for.
	if err := s.Remove(rel); err != nil {
		t.Fatalf("second Remove: %v", err)
	}
	if _, _, err := s.Open(rel); err == nil {
		t.Fatal("Open after Remove should fail")
	}
}

func TestNewRejectsAnUnwritableDirectory(t *testing.T) {
	// A volume that came up read-only should stop the deploy, not turn every
	// upload into a 500 an hour later. Windows ignores the mode bits on a
	// directory, so this can only be proven where they mean something.
	if os.Getuid() == 0 {
		t.Skip("root writes anywhere")
	}
	parent := t.TempDir()
	locked := filepath.Join(parent, "locked")
	if err := os.Mkdir(locked, 0o500); err != nil {
		t.Skipf("cannot create a read-only directory here: %v", err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o700) })

	if _, err := New(filepath.Join(locked, "uploads")); err == nil {
		t.Skip("this filesystem does not enforce directory permissions")
	}
}

func TestExt(t *testing.T) {
	if ext, ok := Ext("image/jpeg"); !ok || ext != ".jpg" {
		t.Fatalf("Ext(image/jpeg) = %q, %v", ext, ok)
	}
	if _, ok := Ext("text/html"); ok {
		t.Fatal("Ext should refuse a type the platform does not store")
	}
}
