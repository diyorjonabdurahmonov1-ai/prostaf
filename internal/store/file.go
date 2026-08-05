package store

import (
	"prostaff/internal/id"
)

// StoredFile is an uploaded image held in the database.
type StoredFile struct {
	ID    string
	Mime  string
	Bytes []byte
}

// maxFileBytes caps a single upload. Picker images at 0.6 quality land well
// under this; anything larger is a client that forgot to compress, and letting
// it through would put megabytes into every profile response.
const maxFileBytes = 3 << 20 // 3 MiB

// SaveFile stores an uploaded image and returns its id.
//
// The bytes go in Postgres rather than on disk on purpose: the server runs on a
// host with an ephemeral filesystem, so a file written next to the binary is
// gone on the next deploy — and a worker's photo that vanishes is worse than
// one that was never uploaded.
func (s *Store) SaveFile(userID, mime string, data []byte) (string, error) {
	if len(data) == 0 {
		return "", ErrBadRequest
	}
	if len(data) > maxFileBytes {
		return "", ErrBadRequest
	}
	switch mime {
	case "image/jpeg", "image/png", "image/webp", "image/heic":
	default:
		return "", ErrBadRequest
	}
	fileID := id.New()
	if _, err := s.db.Exec(
		"INSERT INTO files (id, user_id, mime, bytes) VALUES ($1,$2,$3,$4)",
		fileID, userID, mime, data); err != nil {
		return "", err
	}
	return fileID, nil
}

// GetFile returns a stored file by id.
func (s *Store) GetFile(fileID string) (*StoredFile, bool) {
	var f StoredFile
	f.ID = fileID
	err := s.db.QueryRow("SELECT mime, bytes FROM files WHERE id=$1", fileID).
		Scan(&f.Mime, &f.Bytes)
	if err != nil {
		return nil, false
	}
	return &f, true
}
