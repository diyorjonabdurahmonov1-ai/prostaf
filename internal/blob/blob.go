// Package blob stores uploaded images on the server's own disk.
//
// The alternative — the one this replaces — is a BYTEA column. That was the
// right call on a host whose filesystem is wiped on every deploy, but it means
// every profile photo is carried by Postgres: it bloats `pg_dump` until the
// nightly backup is measured in gigabytes, it pushes real rows out of the page
// cache, and it makes the database the thing you have to resize when what
// actually grew was a folder of JPEGs.
//
// On a VPS the disk survives a restart, so the bytes belong on it. What stays
// in Postgres is the row that names the file — id, mime, path, size — which is
// what a database is for.
//
// Nothing outside changes: `/api/v1/files/{id}` is still the URL, so already
// stored avatar_url strings keep working and the apps need no edit.
package blob

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Store is a directory of uploaded files. A nil *Store is the "not configured"
// case and reports Enabled() == false, so the caller can keep using the
// database without a second code path for "disk turned off".
type Store struct {
	dir string
}

// extByMime is the file suffix given to each accepted image type. Only these
// four are stored — the suffix is cosmetic (the mime type is read back out of
// the database, never guessed from the name) but it makes the upload directory
// something a person can look through with an image viewer.
var extByMime = map[string]string{
	"image/jpeg": ".jpg",
	"image/png":  ".png",
	"image/webp": ".webp",
	"image/heic": ".heic",
}

// New opens dir as a blob store, creating it if it does not exist.
//
// An empty dir means "not configured": no error, and a nil store the caller
// treats as "keep the bytes in Postgres".
//
// Writability is proved here rather than discovered on the first upload —
// a mount that came up read-only should stop the deploy, not quietly turn
// every profile photo into a 500 an hour later.
func New(dir string) (*Store, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, nil
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("blob: resolve %q: %w", dir, err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("blob: create %q: %w", abs, err)
	}
	probe, err := os.CreateTemp(abs, ".writable-*")
	if err != nil {
		return nil, fmt.Errorf("blob: %q is not writable: %w", abs, err)
	}
	name := probe.Name()
	probe.Close()
	os.Remove(name)

	return &Store{dir: abs}, nil
}

// Enabled reports whether uploads should go to disk.
func (s *Store) Enabled() bool { return s != nil && s.dir != "" }

// Dir is the absolute directory being written to, for logs.
func (s *Store) Dir() string {
	if s == nil {
		return ""
	}
	return s.dir
}

// Put writes data under an id and returns the slash-separated path to record in
// the database.
//
// Two details that are not decoration:
//
//   - The name is sharded two levels deep (`f4/7a/f47ac10b-….jpg`). A single
//     directory holding a hundred thousand files is slow to list and slow to
//     open a file in on most filesystems; 256×256 buckets keeps every directory
//     small forever.
//   - The write is to a temporary file that is fsync'd and then renamed. A
//     rename within a directory is atomic, so a reader either finds the whole
//     image or no file at all — never the first half of a JPEG left behind by a
//     process that was killed mid-write.
func (s *Store) Put(id, mime string, data []byte) (string, error) {
	if !s.Enabled() {
		return "", errors.New("blob: store not configured")
	}
	ext, ok := extByMime[mime]
	if !ok {
		return "", fmt.Errorf("blob: unsupported mime %q", mime)
	}
	if len(id) < 4 {
		return "", fmt.Errorf("blob: id %q too short to shard", id)
	}

	rel := id[0:2] + "/" + id[2:4] + "/" + id + ext
	full := s.path(rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return "", fmt.Errorf("blob: create bucket: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(full), ".tmp-*")
	if err != nil {
		return "", fmt.Errorf("blob: create temp: %w", err)
	}
	tmpName := tmp.Name()
	written := false
	defer func() {
		if !written {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		return "", fmt.Errorf("blob: write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return "", fmt.Errorf("blob: sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("blob: close: %w", err)
	}
	if err := os.Rename(tmpName, full); err != nil {
		return "", fmt.Errorf("blob: rename: %w", err)
	}
	written = true
	return rel, nil
}

// Open returns the file at rel together with its stat, for the caller to serve
// and close. The bytes are never read into memory here: an image goes from the
// page cache to the socket, which is what lets a small box serve photos while
// it is also answering API calls.
func (s *Store) Open(rel string) (*os.File, os.FileInfo, error) {
	if !s.Enabled() {
		return nil, nil, errors.New("blob: store not configured")
	}
	full := s.path(rel)
	if full == "" {
		return nil, nil, fmt.Errorf("blob: refusing path %q", rel)
	}
	f, err := os.Open(full)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return f, info, nil
}

// Remove deletes the file at rel. A file that is already gone is not an error —
// the caller is cleaning up, and "not there" is the state it wanted.
func (s *Store) Remove(rel string) error {
	if !s.Enabled() {
		return nil
	}
	full := s.path(rel)
	if full == "" {
		return nil
	}
	if err := os.Remove(full); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// path turns a stored relative path into an absolute one, or returns "" if it
// would leave the store's directory.
//
// Every path handled here was written by Put and read back out of our own
// database, so this can only fire on a corrupted or tampered-with row — which
// is exactly the case worth refusing, because `../../etc/passwd` in a TEXT
// column would otherwise be served to anyone who asked for that file id.
func (s *Store) path(rel string) string {
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return ""
	}
	clean := filepath.Clean(filepath.FromSlash(rel))
	if filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") {
		return ""
	}
	full := filepath.Join(s.dir, clean)
	// Join already cleans, so this is a containment check rather than a string
	// trick: anything that escaped the root no longer has it as a prefix.
	if !strings.HasPrefix(full, s.dir+string(os.PathSeparator)) {
		return ""
	}
	return full
}

// Ext returns the suffix Put would give a file of this mime type, and whether
// the type is one the store accepts at all.
func Ext(mime string) (string, bool) {
	ext, ok := extByMime[mime]
	return ext, ok
}

// interface guard: the API layer serves through io.ReadSeeker.
var _ io.ReadSeeker = (*os.File)(nil)
