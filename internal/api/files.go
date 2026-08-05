package api

import (
	"encoding/base64"
	"net/http"
	"strings"
)

// POST /uploads — turn a picked image into a URL every account can load.
//
// The client sends what `expo-image-picker` already hands it: base64 bytes.
// Before this endpoint existed the app stored the picker's local `file://` URI
// in avatar_url, which renders on the device that chose the photo and nowhere
// else — so nobody ever saw a worker's face but the worker.
//
// Body: { "data": "<base64 or data: URI>", "mime": "image/jpeg" }
// Reply: { "url": "https://host/api/v1/files/<id>", "id": "<id>" }
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	var body struct {
		Data string `json:"data"`
		Mime string `json:"mime"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	raw, mime := splitDataURI(body.Data)
	if body.Mime != "" {
		mime = body.Mime
	}
	if mime == "" {
		mime = "image/jpeg"
	}

	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(raw))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "data must be base64")
		return
	}

	fileID, err := s.store.SaveFile(u.ID, mime, decoded)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":  fileID,
		"url": publicBase(r) + "/api/v1/files/" + fileID,
	})
}

// GET /files/{id} — public on purpose: <Image> tags send no Authorization
// header, and the id is a random uuid, so the URL is the capability.
func (s *Server) handleGetFile(w http.ResponseWriter, r *http.Request) {
	f, ok := s.store.GetFile(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "file not found")
		return
	}
	h := w.Header()
	h.Set("Content-Type", f.Mime)
	// Stored bytes never change under an id, so let every layer keep them.
	h.Set("Cache-Control", "public, max-age=31536000, immutable")
	w.WriteHeader(http.StatusOK)
	w.Write(f.Bytes)
}

// splitDataURI accepts either bare base64 or a full `data:image/png;base64,...`
// URI and returns the payload plus whatever mime type was declared.
func splitDataURI(s string) (payload, mime string) {
	if !strings.HasPrefix(s, "data:") {
		return s, ""
	}
	comma := strings.Index(s, ",")
	if comma < 0 {
		return s, ""
	}
	header := s[len("data:"):comma]
	if semi := strings.Index(header, ";"); semi >= 0 {
		header = header[:semi]
	}
	return s[comma+1:], header
}

// publicBase reconstructs the origin the client reached us on, so the URL we
// hand back works from the phone rather than pointing at the container's own
// idea of localhost. Render terminates TLS in front of us, hence X-Forwarded-*.
func publicBase(r *http.Request) string {
	scheme := "http"
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = strings.Split(proto, ",")[0]
	} else if r.TLS != nil {
		scheme = "https"
	}
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	return scheme + "://" + strings.TrimSpace(host)
}
