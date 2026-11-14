package api

import (
	"net"
	"net/http"
	"strings"
)

// ---- Development features stay on the developer's machine --------------------
//
// DEV_MODE hands the SMS code back in the response and credits wallets without
// a payment. That is what makes local work possible, and it is a complete
// account takeover — reset anybody's password, top up for free — on a server
// where somebody left the flag on. `.env.example` ships it on, and the setting
// defaults to on when it is missing, so "somebody left it on" is the ordinary
// way to get this wrong.
//
// So the flag alone is not enough: the request has to look local as well. Every
// request that came through the public reverse proxy carries X-Forwarded-*
// headers, which the proxy sets itself (Caddy replaces whatever a client sent),
// and a developer calling their own machine carries none.

var forwardedHeaders = []string{
	"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "Forwarded", "X-Real-Ip",
}

// isLocalRequest reports whether the request came straight from this machine or
// its own network, not through a proxy from the internet.
func isLocalRequest(r *http.Request) bool {
	for _, h := range forwardedHeaders {
		if r.Header.Get(h) != "" {
			return false
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate())
}

// devAllowed is DEV_MODE, for a request that is also local.
func (s *Server) devAllowed(r *http.Request) bool {
	return s.cfg.DevMode && isLocalRequest(r)
}

// ---- Headers and limits on every response -----------------------------------

// securityMW adds the headers that stop a browser from doing anything clever
// with an API response, and tells browsers to keep using HTTPS.
//
// The strict policy is for the API only: the documentation page under /swagger
// loads its viewer from a CDN and would not render under it.
func securityMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		}
		if strings.EqualFold(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0], "https") {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

// maxBodyBytes is what any request but a photo upload may send. The handlers
// read a JSON body into memory whole, so without a cap one request could be as
// large as the proxy lets it be — and every handler would have to remember to
// say so itself.
const maxBodyBytes = 1 << 20

// bodyLimitMW caps request bodies. The photo upload sets its own, larger cap
// and is left alone: a second reader wrapped around its reader would apply this
// one on top.
func bodyLimitMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil && r.Body != http.NoBody && !strings.HasSuffix(r.URL.Path, "/uploads") {
			r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		}
		next.ServeHTTP(w, r)
	})
}

// ---- What an upload really is ------------------------------------------------

// sniffImage names the image type the bytes are — one of the types the store
// accepts — or "" for anything else.
func sniffImage(b []byte) string {
	switch {
	case len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return "image/jpeg"
	case len(b) >= 8 && string(b[:8]) == "\x89PNG\r\n\x1a\n":
		return "image/png"
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return "image/webp"
	case len(b) >= 12 && string(b[4:8]) == "ftyp":
		// The HEIF family: a phone's photo, whichever brand it was filed under.
		switch string(b[8:12]) {
		case "heic", "heix", "hevc", "hevx", "heim", "heis", "mif1", "msf1":
			return "image/heic"
		}
	}
	return ""
}
