// Package auth decides who is talking to the API: the operator through a
// session cookie, a script through a bearer token, or the CLI through the
// unix socket. It keeps its files under data/auth/ and never trusts the
// caller's claim about any of it: every check is made here, against what
// is on disk.
package auth

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Iterations is the PBKDF2 work factor for new hashes. 600k rounds of
// SHA-256 is what OWASP recommends in 2023 and costs about a quarter of a
// second on a small VPS: slow enough for an attacker, invisible for one
// login a day. Old hashes keep their own count in the PHC string, so this
// can go up without a migration.
const Iterations = 600_000

const (
	saltLen  = 16
	keyLen   = 32
	phcIdent = "pbkdf2-sha256"
)

// MinPasswordLength is the floor for the operator password. Short
// passwords are the one failure the hash cannot fix.
const MinPasswordLength = 8

// iterations is what HashPassword uses; tests lower it because 600k rounds
// under the race detector takes seconds per login.
var iterations = Iterations

// ErrWeakPassword says the password is too short.
var ErrWeakPassword = fmt.Errorf("password: at least %d characters", MinPasswordLength)

// HashPassword returns a PHC-style string: $pbkdf2-sha256$i=N$salt$hash,
// salt and hash in unpadded base64. The format is what every other PBKDF2
// implementation reads, so a hash made here can be verified elsewhere.
func HashPassword(password string) (string, error) {
	if len(password) < MinPasswordLength {
		return "", ErrWeakPassword
	}
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, iterations, keyLen)
	if err != nil {
		return "", err
	}
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$%s$i=%d$%s$%s", phcIdent, iterations, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// VerifyPassword reports whether password matches the PHC string. A
// malformed string verifies as false, never as an error: the caller
// treats both the same and a corrupted file must not open the door.
func VerifyPassword(phc, password string) bool {
	parts := strings.Split(phc, "$")
	if len(parts) != 5 || parts[0] != "" || parts[1] != phcIdent {
		return false
	}
	iterStr, ok := strings.CutPrefix(parts[2], "i=")
	if !ok {
		return false
	}
	iter, err := strconv.Atoi(iterStr)
	if err != nil || iter <= 0 {
		return false
	}
	b64 := base64.RawStdEncoding
	salt, err := b64.DecodeString(parts[3])
	if err != nil {
		return false
	}
	want, err := b64.DecodeString(parts[4])
	if err != nil || len(want) == 0 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iter, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

// randomToken returns n random bytes as unpadded URL-safe base64.
func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// ErrNotFound is returned for an unknown session, token or setup code.
var ErrNotFound = errors.New("not found")
