package store

import (
	"database/sql"
	"fmt"
	"io"
	"log"
	"time"

	"prostaff/internal/blob"
	"prostaff/internal/id"
)

// StoredFile is an uploaded image. It is one of two things:
//
//   - Path set — the bytes are a file in the upload directory, and the handler
//     streams it straight off the disk.
//   - Bytes set — a row written before uploads moved off the database. Still
//     served, exactly as before, for as long as it is there.
//
// Which one it is is not the caller's decision to make; it is a fact about when
// the photo was uploaded. Both are served under the same URL.
type StoredFile struct {
	ID        string
	Mime      string
	Path      string
	Size      int64
	Bytes     []byte
	CreatedAt time.Time
}

// OnDisk reports whether this file lives in the upload directory.
func (f *StoredFile) OnDisk() bool { return f != nil && f.Path != "" }

// SaveFile stores an uploaded image and returns its id.
//
// With UPLOAD_DIR configured the bytes go to disk and the row records where;
// without it they go into the row, which is what every deployment did before
// there was a disk worth writing to.
func (s *Store) SaveFile(userID, mime string, data []byte) (string, error) {
	if len(data) == 0 {
		return "", ErrBadRequest
	}
	if int64(len(data)) > s.cfg.MaxUploadBytes {
		return "", ErrFileTooLarge
	}
	// blob owns the list of image types the platform accepts, so the check and
	// the file suffix can never drift apart.
	if _, ok := blob.Ext(mime); !ok {
		return "", ErrUnsupportedMedia
	}

	fileID := id.New()

	if !s.blobs.Enabled() {
		if _, err := s.db.Exec(
			"INSERT INTO files (id, user_id, mime, bytes) VALUES ($1,$2,$3,$4)",
			fileID, userID, mime, data); err != nil {
			return "", err
		}
		return fileID, nil
	}

	path, err := s.blobs.Put(fileID, mime, data)
	if err != nil {
		return "", err
	}
	// The row is what makes the file findable, so a file with no row is litter
	// nothing will ever ask for or clean up. Delete it rather than leave it.
	if _, err := s.db.Exec(
		"INSERT INTO files (id, user_id, mime, path, size) VALUES ($1,$2,$3,$4,$5)",
		fileID, userID, mime, path, len(data)); err != nil {
		if rmErr := s.blobs.Remove(path); rmErr != nil {
			log.Printf("files: orphaned %s after failed insert: %v", path, rmErr)
		}
		return "", err
	}
	return fileID, nil
}

// GetFile returns a stored file by id, without reading a disk-backed one into
// memory — Path and Size are enough for the handler to serve it.
func (s *Store) GetFile(fileID string) (*StoredFile, bool) {
	f := StoredFile{ID: fileID}
	// bytes is NULL on every row written since uploads moved to disk, so it has
	// to be scanned through a type that admits that.
	var raw []byte
	err := s.db.QueryRow(
		"SELECT mime, COALESCE(path,''), COALESCE(size,0), bytes, created_at FROM files WHERE id=$1",
		fileID).Scan(&f.Mime, &f.Path, &f.Size, &raw, &f.CreatedAt)
	if err != nil {
		return nil, false
	}
	f.Bytes = raw
	if f.Path == "" && len(f.Bytes) == 0 {
		// Neither a file nor bytes: a row that lost its content. Reporting it
		// missing is honest; serving zero bytes as an image is not.
		return nil, false
	}
	return &f, true
}

// OpenFile opens the disk-backed content of f for serving, together with the
// modification time the handler uses as a cache validator. The caller closes it.
//
// Callers hold a *StoredFile they got from GetFile, so this cannot be asked for
// a path that did not come out of our own database.
func (s *Store) OpenFile(f *StoredFile) (io.ReadSeekCloser, time.Time, error) {
	if f == nil || !f.OnDisk() {
		return nil, time.Time{}, ErrNotFound
	}
	file, info, err := s.blobs.Open(f.Path)
	if err != nil {
		return nil, time.Time{}, err
	}
	return file, info.ModTime(), nil
}

// CountFilesInDB is how many images are still carried by the database rather
// than the disk. Used at startup to say so out loud, because a database quietly
// holding a few gigabytes of JPEGs is exactly the kind of thing nobody notices
// until the nightly backup starts timing out.
func (s *Store) CountFilesInDB() int {
	var n int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM files WHERE COALESCE(path,'') = '' AND bytes IS NOT NULL`,
	).Scan(&n); err != nil {
		return 0
	}
	return n
}

// MigrateFilesToDisk moves every image still held in the database into the
// upload directory, one at a time, and returns how many were moved.
//
// One at a time on purpose: a batch of a hundred three-megabyte photos is a
// third of a gigabyte of Go heap on a box that was chosen for being cheap.
//
// It is safe to run repeatedly and safe to interrupt. Each file is written to
// disk *before* its row stops pointing at the bytes, so a process killed
// halfway leaves rows that are either fully moved or not moved at all — never a
// row naming a file that was never written.
func (s *Store) MigrateFilesToDisk() (int, error) {
	if !s.blobs.Enabled() {
		return 0, fmt.Errorf("files: UPLOAD_MIGRATE needs UPLOAD_DIR to be set")
	}

	rows, err := s.db.Query(
		`SELECT id FROM files WHERE COALESCE(path,'') = '' AND bytes IS NOT NULL ORDER BY created_at`)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var fileID string
		if err := rows.Scan(&fileID); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, fileID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}

	log.Printf("files: moving %d image(s) out of the database into %s", len(ids), s.blobs.Dir())

	moved := 0
	for _, fileID := range ids {
		var mime string
		var data []byte
		err := s.db.QueryRow("SELECT mime, bytes FROM files WHERE id=$1", fileID).Scan(&mime, &data)
		if err == sql.ErrNoRows {
			continue // deleted while we were working; nothing to move
		}
		if err != nil {
			return moved, err
		}
		if len(data) == 0 {
			continue
		}
		if _, ok := blob.Ext(mime); !ok {
			// An old row with a type the platform no longer stores. Leave it in
			// the database, where it is still served, rather than drop it.
			log.Printf("files: leaving %s in the database — unsupported mime %q", fileID, mime)
			continue
		}

		path, err := s.blobs.Put(fileID, mime, data)
		if err != nil {
			return moved, err
		}
		if _, err := s.db.Exec(
			"UPDATE files SET path=$1, size=$2, bytes=NULL WHERE id=$3",
			path, len(data), fileID); err != nil {
			if rmErr := s.blobs.Remove(path); rmErr != nil {
				log.Printf("files: orphaned %s after failed update: %v", path, rmErr)
			}
			return moved, err
		}
		moved++
		if moved%100 == 0 {
			log.Printf("files: moved %d/%d", moved, len(ids))
		}
	}
	return moved, nil
}
