// Package code creates a high-entropy, copyable invitation. The relay only
// sees independently derived identifiers and authorization tokens.
package code

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
)

type Code struct{ secret [32]byte }

// New never returns a code whose text starts with '-': command-line parsers
// would read it as an option. That rejects 1 in 64 draws and costs 6 bits.
func New() (Code, error) {
	var c Code
	for {
		if _, err := rand.Read(c.secret[:]); err != nil {
			return c, err
		}
		if !strings.HasPrefix(c.String(), "-") {
			return c, nil
		}
	}
}

func (c Code) String() string { return base64.RawURLEncoding.EncodeToString(c.secret[:]) }

func ValidRoom(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == 16 && s == strings.ToLower(s)
}

func Parse(s string) (Code, error) {
	var c Code
	s = strings.TrimSpace(s)
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(b) != len(c.secret) || base64.RawURLEncoding.EncodeToString(b) != s {
		return c, errors.New("invalid jand code")
	}
	copy(c.secret[:], b)
	return c, nil
}

func (c Code) Derive(label string) [32]byte {
	m := hmac.New(sha256.New, c.secret[:])
	m.Write([]byte("handoff/0.2/" + label))
	var out [32]byte
	copy(out[:], m.Sum(nil))
	return out
}

func (c Code) Room() string {
	id := c.Derive("room")
	return hex.EncodeToString(id[:16])
}

func (c Code) Token(label string) string {
	t := c.Derive(label)
	return base64.RawURLEncoding.EncodeToString(t[:])
}
