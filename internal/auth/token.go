// Package auth authenticates smart-HTTP requests with personal access tokens.
// It authorizes them per repo against the ACL table, as
// docs/architecture/auth.md describes.
//
// A PAT is a `selector.secret` pair (see docs/architecture/storage.md). The
// selector is a lookup key that is not secret. Only the SHA-256 of the secret
// is stored, and that hash is the verifier. Verification looks up the row by
// selector, then compares the verifier in constant time. So neither a timing
// side-channel nor a database leak yields a usable token.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
)

// Sentinels that distinguish the two rejection modes, so the transport layer
// can answer with the auth challenge that git expects.
var (
	// ErrUnauthorized is a missing or invalid token. Answer 401 with a challenge.
	ErrUnauthorized = errors.New("auth: unauthorized")
	// ErrForbidden is a valid token without the required permission. Answer 403.
	ErrForbidden = errors.New("auth: forbidden")
)

const (
	// tokenPrefix marks gitmote PATs (like GitHub's ghp_), so a leaked token is
	// recognizable and secret-scanners can match it.
	tokenPrefix = "gmt_"
	// selectorBytes and secretBytes size the two halves. The 128-bit selector
	// gives a collision-free lookup key. The 256-bit secret backs the verifier.
	selectorBytes = 16
	secretBytes   = 32
)

// Mint generates a new personal access token. It returns the raw token, which
// the user sees exactly once. It also returns the selector and verifier to
// persist through meta.CreateToken.
func Mint() (raw, selector, verifier string, err error) {
	sel, err := randHex(selectorBytes)
	if err != nil {
		return "", "", "", err
	}
	secret, err := randHex(secretBytes)
	if err != nil {
		return "", "", "", err
	}
	raw = tokenPrefix + sel + "." + secret
	return raw, sel, hashSecret(secret), nil
}

// split parses a raw token into its selector and secret halves. It reports
// ok=false for anything not shaped like a gitmote PAT.
func split(raw string) (selector, secret string, ok bool) {
	rest, found := strings.CutPrefix(raw, tokenPrefix)
	if !found {
		return "", "", false
	}
	selector, secret, found = strings.Cut(rest, ".")
	if !found || selector == "" || secret == "" {
		return "", "", false
	}
	return selector, secret, true
}

// hashSecret is the verifier derivation: SHA-256 of the secret half. The secret
// is a 256-bit random string, so a plain hash (not a slow KDF) is sufficient.
func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
