// Package secrets encrypts per-repo CI secrets at rest with a server-held master
// key, so a compromise of the S3 replica / DB snapshot (the litestream WAL, the
// object bucket) does not leak secret values — the master key lives only in the
// process env, not in the replicated DB. It is **not** a defense against a
// compromised running server, which holds the key by necessity to inject secrets
// into the runner (see docs/architecture/safety.md and tasks/16-ci.md §Stage 0 #3).
//
// keyring.go is pure crypto: no meta, no I/O beyond reading keys from the env at
// construction — unit-testable in isolation. The meta-backed store and the
// encrypt/decrypt service live alongside it.
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
	// keyEnvPrefix is the env-var stem for a master key; the version number
	// follows (GITMOTE_CI_SECRET_KEY_V1, …). The value is base64 of 32 raw bytes.
	keyEnvPrefix = "GITMOTE_CI_SECRET_KEY_V"
	masterKeyLen = 32 // AES-256
	ivLen        = 12 // GCM standard nonce
	// subkeyInfo is the HKDF info stem; the repo id follows, giving a distinct
	// per-repo subkey from one master (repo isolation for free).
	subkeyInfo = "ci-secret:"
)

// Envelope is an encrypted secret at rest: the key version that sealed it, the
// random IV, and the AES-GCM ciphertext (which carries the auth tag).
type Envelope struct {
	Version int
	IV      []byte
	CT      []byte
}

// Keyring holds the server-held master keys, one per version. The highest
// version present is *current*: new secrets seal under it, while existing
// envelopes decrypt under their own version — lazy, O(1) rotation with no
// re-encryption. It is immutable after construction, so safe for concurrent use.
type Keyring struct {
	keys    map[int][masterKeyLen]byte
	current int
}

// NewKeyringFromEnv builds the keyring from GITMOTE_CI_SECRET_KEY_V<n> env vars
// (base64 of 32 bytes each). With none set it returns a disabled keyring
// (Enabled() == false) rather than an error, so an instance without CI secrets
// runs unchanged. A present-but-malformed key is a hard error — fail loud rather
// than silently drop a key version.
func NewKeyringFromEnv() (*Keyring, error) {
	kr := &Keyring{keys: map[int][masterKeyLen]byte{}}
	for _, kv := range os.Environ() {
		name, val, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(name, keyEnvPrefix) {
			continue
		}
		if strings.TrimSpace(val) == "" {
			continue // an empty/cleared var is "unset", not a malformed key
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

// Enabled reports whether any master key is configured. When false, Encrypt
// fails and the secrets UI/injection are inert.
func (k *Keyring) Enabled() bool { return len(k.keys) > 0 }

// Encrypt seals plaintext for (repoID, name) under the current key version. The
// IV is fresh per call, so two encryptions of the same value differ. The AAD
// binds the envelope to (repoID, name, version): a ciphertext moved to a
// different repo/name/version fails to open.
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

// Decrypt opens an envelope for (repoID, name). It fails closed: an absent key
// version, a bad IV, or a tag mismatch (wrong repo/name/version, or tampering)
// all return an error — never a silent empty value or a panic. The error carries
// the repo/name (not secret) but never the plaintext.
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
