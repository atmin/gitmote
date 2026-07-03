package secrets

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/atmin/gitmote/internal/meta"
)

// Store is the meta slice the service reads and writes. *meta.Metadata satisfies
// it. It carries only sealed envelopes — never plaintext.
type Store interface {
	SetSecret(ctx context.Context, repoID int64, name string, version int, iv, ct []byte) error
	ListSecretNames(ctx context.Context, repoID int64) ([]string, error)
	GetSecrets(ctx context.Context, repoID int64) ([]meta.CISecret, error)
	DeleteSecret(ctx context.Context, repoID int64, name string) error
}

// Service is the CI-secrets hub: it seals on write and opens on read, over the
// keyring and the meta store. The admin UI uses Set/List/Delete/Enabled; the CI
// dispatcher uses Secrets to build the trigger env.
type Service struct {
	kr    *Keyring
	store Store
}

// NewService binds a keyring to a store.
func NewService(kr *Keyring, store Store) *Service {
	return &Service{kr: kr, store: store}
}

// Enabled reports whether a master key is configured (so the UI can gate the
// set form and injection is meaningful).
func (s *Service) Enabled() bool { return s.kr.Enabled() }

// nameRe constrains a secret name to a shell/env-safe identifier so it can be
// injected as a runner env var without quoting surprises.
var nameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// validName rejects malformed names and names that would shadow the runner's own
// injected env (GITMOTE_*, WORKER_SECRET).
func validName(name string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("name %q must match [A-Za-z_][A-Za-z0-9_]*", name)
	}
	if strings.HasPrefix(name, "GITMOTE_") || name == "WORKER_SECRET" {
		return fmt.Errorf("name %q is reserved", name)
	}
	return nil
}

// SetSecret validates the name, seals the value under the current key version,
// and upserts the envelope. The plaintext never leaves this call.
func (s *Service) SetSecret(ctx context.Context, repoID int64, name, value string) error {
	if err := validName(name); err != nil {
		return err
	}
	env, err := s.kr.Encrypt(repoID, name, value)
	if err != nil {
		return err
	}
	return s.store.SetSecret(ctx, repoID, name, env.Version, env.IV, env.CT)
}

// ListSecretNames returns a repo's secret names (never values).
func (s *Service) ListSecretNames(ctx context.Context, repoID int64) ([]string, error) {
	return s.store.ListSecretNames(ctx, repoID)
}

// DeleteSecret removes a repo's secret by name (idempotent).
func (s *Service) DeleteSecret(ctx context.Context, repoID int64, name string) error {
	return s.store.DeleteSecret(ctx, repoID, name)
}

// Secrets decrypts a repo's secrets into a name→value map for the trigger env.
// It fails closed: if any envelope can't be opened (missing key version,
// tampering), it returns the error rather than a partial map, so the dispatcher
// can decide to run without secrets rather than with a silently incomplete set.
func (s *Service) Secrets(ctx context.Context, repoID int64) (map[string]string, error) {
	stored, err := s.store.GetSecrets(ctx, repoID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(stored))
	for _, e := range stored {
		v, err := s.kr.Decrypt(repoID, e.Name, Envelope{Version: e.Version, IV: e.IV, CT: e.CT})
		if err != nil {
			return nil, err
		}
		out[e.Name] = v
	}
	return out, nil
}
