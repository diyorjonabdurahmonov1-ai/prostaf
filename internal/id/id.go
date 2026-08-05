// Package id generates RFC 4122 version 4 UUIDs using only the standard library.
package id

import (
	"crypto/rand"
	"encoding/hex"
)

// New returns a random UUIDv4 string (e.g. "f47ac10b-58cc-4372-a567-0e02b2c3d479").
func New() string {
	var b [16]byte
	// crypto/rand.Read never returns an error on a healthy system; if the OS
	// entropy source fails the program cannot safely continue, so we ignore it.
	_, _ = rand.Read(b[:])
	// Set version (4) and variant (RFC 4122) bits.
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80

	var out [36]byte
	hex.Encode(out[0:8], b[0:4])
	out[8] = '-'
	hex.Encode(out[9:13], b[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], b[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], b[8:10])
	out[23] = '-'
	hex.Encode(out[24:36], b[10:16])
	return string(out[:])
}
