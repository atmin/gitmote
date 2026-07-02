package webui

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSessionRoundTrip(t *testing.T) {
	s := &sessions{key: []byte("k")}
	now := time.Unix(1_700_000_000, 0)

	rec := httptest.NewRecorder()
	s.issue(rec, httptest.NewRequest(http.MethodGet, "/", nil), 42, now)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, c := range rec.Result().Cookies() {
		r.AddCookie(c)
	}
	uid, ok := s.verify(r, now.Add(time.Minute))
	if !ok || uid != 42 {
		t.Fatalf("verify = %d, %v; want 42, true", uid, ok)
	}

	// Expired session is rejected.
	if _, ok := s.verify(r, now.Add(sessionTTL+time.Second)); ok {
		t.Error("expired session verified")
	}

	// Tampered payload (different key) is rejected.
	other := &sessions{key: []byte("different")}
	if _, ok := other.verify(r, now.Add(time.Minute)); ok {
		t.Error("session verified under a different signing key")
	}
}
