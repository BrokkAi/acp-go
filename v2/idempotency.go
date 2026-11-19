package v2

import (
	"crypto/rand"
	"fmt"
)

// NewIdempotencyKey returns a new key for one session/prompt submission: a
// random version 4 UUID (RFC 9562) in its 36-character hyphenated lowercase
// form, as Rust's IdempotencyKey::generate makes one, so it is always a valid
// key. See Connection.PromptContentWithKey.
func NewIdempotencyKey() IdempotencyKey {
	var u [16]byte
	_, _ = rand.Read(u[:])  // crypto/rand.Read never fails
	u[6] = u[6]&0x0f | 0x40 // version 4
	u[8] = u[8]&0x3f | 0x80 // RFC 9562 variant
	return IdempotencyKey(fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16]))
}
