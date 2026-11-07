// Package secrets encrypts per-repo CI secrets at rest with a master key that
// the server holds. A compromise of the S3 replica or a database snapshot then
// does not leak secret values. Examples are the litestream WAL or the object
// bucket.
// The master key lives only in the process env, not in the replicated
// database. It is **not** a defense against a compromised running server. The
// server must hold the key to inject secrets into the runner (see
// docs/architecture/safety.md and docs/architecture/ci.md).
//
// keyring.go is pure crypto. It uses no meta and does no I/O beyond reading
// keys from the env at construction. So it is unit-testable in isolation. The
// store backed by meta and the service that encrypts and decrypts live beside
// it.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

const (
	// keyEnvPrefix is the env var stem for a master key. The version number
	// follows it, as in GITMOTE_CI_SECRET_KEY_V1. The value is base64 of 32
	// raw bytes.
	keyEnvPrefix = "GITMOTE_CI_SECRET_KEY_V"
	masterKeyLen = 32 // AES-256
	ivLen        = 12 // GCM standard nonce
	// subkeyInfo is the HKDF info stem. The repo id follows it, which gives a
	// distinct per-repo subkey from one master key. This isolates the repos
	// at no extra cost.
	subkeyInfo = "ci-secret:"
)

// Envelope is an encrypted secret at rest: the key version that sealed it, the
// random IV, and the AES-GCM ciphertext (which carries the auth tag).
type Envelope struct {
	Version int
	IV      []byte
	CT      []byte
}

// Keyring holds the server's master keys, one per version. The highest version
// present is *current*. New secrets seal under it. Existing envelopes decrypt
// under their own version. This gives lazy, O(1) rotation with no
// re-encryption. Keyring is immutable after construction, so it is safe for
// concurrent use.
type Keyring struct {
	keys    map[int][masterKeyLen]byte
	current int
}

// NewKeyringFromEnv builds the keyring from the GITMOTE_CI_SECRET_KEY_V<n> env
// vars. Each one holds base64 of 32 bytes. When none is set, it returns a
// disabled keyring (Enabled() == false), not an error. So an instance without
// CI secrets runs unchanged. A key that is present but malformed is a hard
// error. Failing loudly is better than silently dropping a key version.
func NewKeyringFromEnv() (*Keyring, error) {
	kr := &Keyring{keys: map[int][masterKeyLen]byte{}}
	for _, kv := range os.Environ() {
		name, val, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(name, keyEnvPrefix) {
			continue
		}
		if strings.TrimSpace(val) == "" {
			continue // an empty or cleared var is "unset", not a malformed key
		}
		v, err := strconv.Atoi(strings.TrimPrefix(name, keyEnvPrefix))
		if err != nil || v < 1 {
			return nil, fmt.Errorf("secrets: %s has a non-numeric key version", name)
		}
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(val))
		if err != nil {
			return nil, fmt.Errorf("secrets: %s is not valid base64: %w", name, err)
		}
		if len(raw) != masterKeyLen {
			return nil, fmt.Errorf("secrets: %s must decode to %d bytes, got %d", name, masterKeyLen, len(raw))
		}
		var k [masterKeyLen]byte
		copy(k[:], raw)
		kr.keys[v] = k
		if v > kr.current {
			kr.current = v
		}
	}
	return kr, nil
}

// Enabled reports whether any master key is configured. When it is false,
// Encrypt fails, and the secrets UI and the injection are inert.
func (k *Keyring) Enabled() bool { return len(k.keys) > 0 }

// Encrypt seals plaintext for (repoID, name) under the current key version. The
// IV is fresh per call, so two encryptions of the same value differ. The AAD
// binds the envelope to (repoID, name, version). A ciphertext moved to a
// different repo, name or version fails to open.
func (k *Keyring) Encrypt(repoID int64, name, plaintext string) (Envelope, error) {
	if !k.Enabled() {
		return Envelope{}, errors.New("secrets: no master key configured (set GITMOTE_CI_SECRET_KEY_V1)")
	}
	gcm, err := k.gcm(k.current, repoID)
	if err != nil {
		return Envelope{}, err
	}
	iv := make([]byte, ivLen)
	if _, err := rand.Read(iv); err != nil {
		return Envelope{}, err
	}
	ct := gcm.Seal(nil, iv, []byte(plaintext), aad(repoID, name, k.current))
	return Envelope{Version: k.current, IV: iv, CT: ct}, nil
}

// Decrypt opens an envelope for (repoID, name). It fails closed. Each of these
// cases returns an error, never a silent empty value or a panic:
//   - an absent key version
//   - a bad IV
//   - a tag mismatch, from a wrong repo, name or version, or from tampering
//
// The error carries the repo and the name, which are not secret. It never
// carries the plaintext.
func (k *Keyring) Decrypt(repoID int64, name string, e Envelope) (string, error) {
	if len(e.IV) != ivLen {
		return "", fmt.Errorf("secrets: bad IV length %d for repo %d secret %q", len(e.IV), repoID, name)
	}
	gcm, err := k.gcm(e.Version, repoID)
	if err != nil {
		return "", err
	}
	pt, err := gcm.Open(nil, e.IV, e.CT, aad(repoID, name, e.Version))
	if err != nil {
		return "", fmt.Errorf("secrets: decrypt failed for repo %d secret %q: %w", repoID, name, err)
	}
	return string(pt), nil
}

// gcm derives the per-repo subkey for the given master version via HKDF and
// returns an AES-256-GCM AEAD over it. An unknown version fails closed.
func (k *Keyring) gcm(version int, repoID int64) (cipher.AEAD, error) {
	master, ok := k.keys[version]
	if !ok {
		return nil, fmt.Errorf("secrets: key version %d is not configured (set %s%d) — cannot process this secret", version, keyEnvPrefix, version)
	}
	sub, err := hkdf.Key(sha256.New, master[:], nil, subkeyInfo+strconv.FormatInt(repoID, 10), masterKeyLen)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(sub)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// aad binds an envelope to its (repoID, name, version). name is NUL-free (the
// service validates it), so the concatenation is unambiguous.
func aad(repoID int64, name string, version int) []byte {
	return []byte(strconv.FormatInt(repoID, 10) + "\x00" + name + "\x00" + strconv.Itoa(version))
}
