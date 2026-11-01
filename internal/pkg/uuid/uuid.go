// Package uuid generates random UUIDs.
package uuid

import (
	"crypto/rand"
	"fmt"
)

// New returns a random RFC 4122 version 4 UUID.
func New() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // never fails
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}
