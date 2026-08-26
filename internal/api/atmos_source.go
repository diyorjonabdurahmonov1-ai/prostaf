package api

import (
	"net"
	"net/http"
	"strings"
)

// Who a callback actually came from, and whether they are allowed to.
//
// Atmos asks merchants to accept callbacks only from its own range
// (92.63.207.0/24). That is worth having, but it is the *second* lock: the
// signature in handleAtmosCallback is what genuinely protects the endpoint, and
// it is checked whether or not an allowlist is configured. The order matters if
// this is ever inverted — an address is a much weaker claim than a hash of a
// secret, and it is the one an operator is likely to get wrong.

// callbackSourceIP reports the address a request really arrived from.
//
// Behind Caddy the peer is Caddy, so X-Forwarded-For carries the original. The
// *last* entry is the one to trust, not the first: a proxy appends the address
// it saw, so anything a caller invented sits to the left of it. Reading the
// first entry — the obvious thing, and what most examples show — would let
// anybody put a permitted address at the front of the header and walk through
// the allowlist below.
//
// Returns "" when nothing parseable is available, which the check treats as not
// permitted rather than as permitted.
func callbackSourceIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		parts := strings.Split(fwd, ",")
		last := strings.TrimSpace(parts[len(parts)-1])
		if ip := net.ParseIP(last); ip != nil {
			return ip.String()
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(strings.TrimSpace(host)); ip != nil {
		return ip.String()
	}
	return ""
}

// parseCIDRs turns the configured list into networks, ignoring what it cannot
// read. A bare address is accepted as itself, since "92.63.207.79" is the
// obvious thing to type and refusing it would only invite a wrong /24.
func parseCIDRs(list string) []*net.IPNet {
	var nets []*net.IPNet
	for _, part := range strings.Split(list, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if _, n, err := net.ParseCIDR(part); err == nil {
			nets = append(nets, n)
			continue
		}
		if ip := net.ParseIP(part); ip != nil {
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			nets = append(nets, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
		}
	}
	return nets
}

// callbackAllowed reports whether this source may post a callback.
//
// With no allowlist configured everything is allowed, which is the default and
// leaves the signature as the only gate — deliberately, because an allowlist
// that is subtly wrong fails in the expensive direction: the gateway is turned
// away, the payer's money has already moved, and nothing in the app says so.
func (s *Server) callbackAllowed(ip string) bool {
	nets := s.atmosCallbackNets
	if len(nets) == 0 {
		return true
	}
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	for _, n := range nets {
		if n.Contains(parsed) {
			return true
		}
	}
	return false
}
