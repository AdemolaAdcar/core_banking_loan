package service

import (
	"crypto/rand"
	"fmt"
)

// newUUIDv4 generates a random (v4) UUID using crypto/rand, without
// pulling in an external UUID dependency for something this small.
func newUUIDv4() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand.Read failing is effectively unrecoverable (no
		// entropy source available) -- this is one of the rare cases in
		// this codebase where a panic is preferable to threading an
		// error through every ID-generating call site for a condition
		// that, in practice, never happens on any real deployment target.
		panic(fmt.Sprintf("service: crypto/rand unavailable: %v", err))
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
