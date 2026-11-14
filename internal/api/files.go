package api

import (
	"bytes"
	"encoding/base64"
	"log"
	"net/http"
	"strings"

	"prostaff/internal/store"
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

	// The size cap used to apply to the *decoded* image, which is a cap that
	// arrives too late: the body is read into memory, and the base64 of it is
	// read into memory again, before anything measures either. A request
	// claiming to be a two-gigabyte photo would be two gigabytes of heap on a
	// server that has four, and no reverse proxy limits bodies by default.
	//
	// So the body is capped where it is read. Base64 costs four bytes for every
	// three, and the JSON around it is a few dozen more; the slack covers both.
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxUploadBytes*4/3+4096)

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
	// The label the client wrote says what it hopes the bytes are. What they
	// are is read from the bytes: a page or a script sent as "image/jpeg" used
	// to be stored and served back under that label from this domain.
	real := sniffImage(decoded)
	if real == "" {
		writeStoreError(w, store.ErrUnsupportedMedia)
		return
	}
	mime = real

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
//
// Served through http.ServeContent for what it handles that a plain Write does
// not: a conditional request answered with 304 instead of the image again, and
// a range request, which is how a phone resumes a photo whose download was cut
// off by a lift. For a file on disk it also means the bytes go from the page
// cache to the socket without passing through this process.
func (s *Server) handleGetFile(w http.ResponseWriter, r *http.Request) {
	f, ok := s.store.GetFile(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "file not found")
		return
	}

	h := w.Header()
	h.Set("Content-Type", f.Mime)
	// An image is only ever an image: nothing in it may run if somebody opens
	// the link in a browser, whatever the bytes turn out to contain.
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	// Stored bytes never change under an id, so let every layer keep them —
	// and for the same reason the id itself is a valid ETag.
	h.Set("Cache-Control", "public, max-age=31536000, immutable")
	h.Set("ETag", `"`+f.ID+`"`)

	if f.OnDisk() {
		content, modTime, err := s.store.OpenFile(f)
		if err != nil {
			// The row says there is a file and there is not. Almost always a
			// restore that brought the database back without the upload
			// directory, so name the path — it is the one detail that turns a
			// screenful of 404s into an obvious missing volume.
			log.Printf("files: %s missing from disk (%s): %v", f.ID, f.Path, err)
			writeError(w, http.StatusNotFound, "not_found", "file not found")
			return
		}
		defer content.Close()
		http.ServeContent(w, r, "", modTime, content)
		return
	}

	http.ServeContent(w, r, "", f.CreatedAt, bytes.NewReader(f.Bytes))
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
// idea of localhost.
//
// TLS is terminated in front of us — by Render, or by Caddy/nginx on a server
// of our own — hence X-Forwarded-*. A proxy that forgets to send
// X-Forwarded-Proto makes every uploaded URL an `http://` one, and Android
// blocks cleartext by default: the upload succeeds and the photo never loads.
// See the deploy Caddyfile, which sets both headers.
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
