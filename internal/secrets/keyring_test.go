package secrets

import (
	"encoding/base64"
	"strings"
	"testing"
)

// b64key returns a distinct base64 32-byte key seeded by n.
func b64key(n byte) string {
	var k [masterKeyLen]byte
	for i := range k {
		k[i] = n + byte(i)
	}
	return base64.StdEncoding.EncodeToString(k[:])
}

// keyringEnv sets GITMOTE_CI_SECRET_KEY_V<v> for each (v→key) and builds a
// keyring from the env.
func keyringEnv(t *testing.T, keys map[int]string) *Keyring {
	t.Helper()
	// Clear any inherited key vars so the test is hermetic.
	for v := 1; v <= 9; v++ {
		t.Setenv(keyEnvPrefix+string(rune('0'+v)), "")
	}
	for v, k := range keys {
		t.Setenv(keyEnvPrefix+string(rune('0'+v)), k)
	}
	kr, err := NewKeyringFromEnv()
	if err != nil {
		t.Fatalf("NewKeyringFromEnv: %v", err)
	}
	return kr
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	kr := keyringEnv(t, map[int]string{1: b64key(1)})
	env, err := kr.Encrypt(7, "TOKEN", "s3cr3t-value")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if env.Version != 1 || len(env.IV) != ivLen || len(env.CT) == 0 {
		t.Fatalf("envelope = %+v, want v1 with a 12-byte IV and ciphertext", env)
	}
	got, err := kr.Decrypt(7, "TOKEN", env)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if got != "s3cr3t-value" {
		t.Errorf("decrypted = %q, want the original", got)
	}
}

func TestWrongAADFails(t *testing.T) {
	kr := keyringEnv(t, map[int]string{1: b64key(1)})
	env, _ := kr.Encrypt(7, "TOKEN", "v")

	if _, err := kr.Decrypt(8, "TOKEN", env); err == nil {
		t.Error("decrypt under a different repoID succeeded, want auth-tag failure")
	}
	if _, err := kr.Decrypt(7, "OTHER", env); err == nil {
		t.Error("decrypt under a different name succeeded, want auth-tag failure")
	}
	tampered := Envelope{Version: 1, IV: env.IV, CT: append([]byte{env.CT[0] ^ 0xff}, env.CT[1:]...)}
	if _, err := kr.Decrypt(7, "TOKEN", tampered); err == nil {
		t.Error("decrypt of tampered ciphertext succeeded, want auth-tag failure")
	}
}

func TestFreshIVPerEncryption(t *testing.T) {
	kr := keyringEnv(t, map[int]string{1: b64key(1)})
	a, _ := kr.Encrypt(7, "TOKEN", "same")
	b, _ := kr.Encrypt(7, "TOKEN", "same")
	if string(a.IV) == string(b.IV) {
		t.Error("two encryptions reused the IV")
	}
	if string(a.CT) == string(b.CT) {
		t.Error("two encryptions of the same plaintext produced identical ciphertext")
	}
}

func TestMissingVersionFailsClosed(t *testing.T) {
	// Encrypt under V1, then present a keyring that only knows V2.
	env, _ := keyringEnv(t, map[int]string{1: b64key(1)}).Encrypt(7, "TOKEN", "v")
	only2 := keyringEnv(t, map[int]string{2: b64key(2)})
	if _, err := only2.Decrypt(7, "TOKEN", env); err == nil {
		t.Error("decrypt with the sealing key version absent succeeded, want a fail-closed error")
	} else if !strings.Contains(err.Error(), "version 1") {
		t.Errorf("error = %v, want it to name the missing version", err)
	}
}

func TestRotation(t *testing.T) {
	// A secret written under V1 must still decrypt after V2 is added and becomes
	// current; new writes then use V2.
	old := keyringEnv(t, map[int]string{1: b64key(1)})
	e1, _ := old.Encrypt(7, "TOKEN", "old-value")

	rotated := keyringEnv(t, map[int]string{1: b64key(1), 2: b64key(2)})
	got, err := rotated.Decrypt(7, "TOKEN", e1)
	if err != nil || got != "old-value" {
		t.Fatalf("V1 secret after rotation = %q, %v; want the original", got, err)
	}
	e2, err := rotated.Encrypt(7, "TOKEN", "new-value")
	if err != nil {
		t.Fatalf("Encrypt after rotation: %v", err)
	}
	if e2.Version != 2 {
		t.Errorf("new write sealed under v%d, want v2 (current)", e2.Version)
	}
}

func TestDisabledKeyring(t *testing.T) {
	kr := keyringEnv(t, nil)
	if kr.Enabled() {
		t.Error("keyring with no keys reports enabled")
	}
	if _, err := kr.Encrypt(7, "TOKEN", "v"); err == nil {
		t.Error("Encrypt with no key configured succeeded, want an error")
	}
}

func TestBadKeyEnvIsHardError(t *testing.T) {
	t.Setenv(keyEnvPrefix+"1", "not-base64!!")
	if _, err := NewKeyringFromEnv(); err == nil {
		t.Error("malformed base64 key accepted, want a hard error")
	}
	t.Setenv(keyEnvPrefix+"1", base64.StdEncoding.EncodeToString([]byte("too-short")))
	if _, err := NewKeyringFromEnv(); err == nil {
		t.Error("wrong-length key accepted, want a hard error")
	}
}
