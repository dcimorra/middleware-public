package access

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
)

// A site password is stored in the manifest as a PHC-style string:
//
//	$pbkdf2-sha256$<iterations>$<salt b64>$<hash b64>
//
// PBKDF2 rather than argon2 or bcrypt because it is the one the standard
// library ships, and a manifest edited by hand must stay readable: the
// format names its own parameters, so the cost can rise later without
// invalidating what is already written.
const (
	hashID = "pbkdf2-sha256"
	// hashIterations is tuned to a few milliseconds per check: every
	// request with HTTP basic auth pays it (through a small cache), so
	// it cannot be the hundreds of milliseconds a login form would bear.
	hashIterations = 50_000
	saltLen        = 16
	keyLen         = 32
)

var b64 = base64.RawStdEncoding

// HashPassword returns the string to store for plain. It never fails for
// a non-empty password; an empty one is rejected because an empty hash
// field means "no password" and the two must not be confused.
func HashPassword(plain string) (string, error) {
	if plain == "" {
		return "", errors.New("password: empty")
	}
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, plain, salt, hashIterations, keyLen)
	if err != nil {
		return "", err
	}
	return "$" + hashID + "$" + strconv.Itoa(hashIterations) + "$" + b64.EncodeToString(salt) + "$" + b64.EncodeToString(key), nil
}

// CheckPassword reports whether plain matches a stored hash. A malformed
// hash never matches: refusing everyone is safer than letting everyone in
// when a manifest has been hand-edited badly.
func CheckPassword(hash, plain string) bool {
	if hash == "" || plain == "" {
		return false
	}
	parts := strings.Split(hash, "$")
	if len(parts) != 5 || parts[0] != "" || parts[1] != hashID {
		return false
	}
	iter, err := strconv.Atoi(parts[2])
	if err != nil || iter <= 0 || iter > 10_000_000 {
		return false
	}
	salt, err := b64.DecodeString(parts[3])
	if err != nil {
		return false
	}
	want, err := b64.DecodeString(parts[4])
	if err != nil || len(want) == 0 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, plain, salt, iter, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

// IsHash reports whether s looks like something HashPassword produced, so
// an API can tell a plain password it must hash from one already stored.
func IsHash(s string) bool {
	return strings.HasPrefix(s, "$"+hashID+"$") && strings.Count(s, "$") == 4
}
