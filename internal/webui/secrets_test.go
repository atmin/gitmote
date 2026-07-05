package webui

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestSecretsGoldenPath(t *testing.T) {
	x := newHarness(t)
	ctx := context.Background()
	session := x.login(x.mintTokenFor(x.admin.ID))

	repo, err := x.md.CreateRepo(ctx, "app", "main")
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}

	// Set a secret. The value must never appear in the response.
	const value = "hunter2-NEEDLE"
	rec := x.do(http.MethodPost, "/app/secrets",
		url.Values{"name": {"API_TOKEN"}, "value": {value}}, session)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "saved secret API_TOKEN") {
		t.Fatalf("set secret: %d (%s)", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), value) {
		t.Error("set-secret response echoed the value")
	}

	// It is stored as an envelope (name listable, value not in plaintext).
	stored, _ := x.md.GetSecrets(ctx, repo.ID)
	if len(stored) != 1 || stored[0].Name != "API_TOKEN" {
		t.Fatalf("stored = %+v, want one API_TOKEN envelope", stored)
	}
	if strings.Contains(string(stored[0].CT), value) {
		t.Error("stored ciphertext contains the plaintext value")
	}

	// The list page shows the name but never the value.
	rec = x.do(http.MethodGet, "/app/secrets", nil, session)
	if !strings.Contains(rec.Body.String(), "API_TOKEN") {
		t.Errorf("list page missing the secret name (body: %s)", rec.Body)
	}
	if strings.Contains(rec.Body.String(), value) {
		t.Error("list page leaked the secret value")
	}

	// Delete it.
	rec = x.do(http.MethodPost, "/app/secrets/delete",
		url.Values{"name": {"API_TOKEN"}}, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete secret: %d (%s)", rec.Code, rec.Body)
	}
	if names, _ := x.md.ListSecretNames(ctx, repo.ID); len(names) != 0 {
		t.Errorf("names after delete = %v, want empty", names)
	}
}

func TestSecretsRejectsReservedName(t *testing.T) {
	x := newHarness(t)
	ctx := context.Background()
	session := x.login(x.mintTokenFor(x.admin.ID))
	if _, err := x.md.CreateRepo(ctx, "app", "main"); err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}

	rec := x.do(http.MethodPost, "/app/secrets",
		url.Values{"name": {"WORKER_SECRET"}, "value": {"x"}}, session)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "reserved") {
		t.Errorf("reserved name: %d (%s), want a rejection message", rec.Code, rec.Body)
	}
}

func TestSecretsRequireAdmin(t *testing.T) {
	x := newHarness(t)

	// Unauthenticated GET redirects to login; POST is 401 — the panel is gated
	// by requireAdmin, which runs before the repo is even looked up.
	if rec := x.do(http.MethodGet, "/app/secrets", nil, nil); rec.Code != http.StatusSeeOther {
		t.Errorf("unauth GET /app/secrets = %d, want 303 redirect", rec.Code)
	}
	rec := x.do(http.MethodPost, "/app/secrets",
		url.Values{"name": {"K"}, "value": {"v"}}, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unauth POST /app/secrets = %d, want 401", rec.Code)
	}
}
