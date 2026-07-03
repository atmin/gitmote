package scaleway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTriggerPostsStartRequest(t *testing.T) {
	var (
		gotPath  string
		gotToken string
		gotBody  map[string]map[string]string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotToken = r.Header.Get("X-Auth-Token")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Point the client at the test server by overriding the base URL via a client
	// that rewrites the host: simplest is to build the client and swap its
	// transport to redirect to srv.
	c := NewJobsClient("secret-key", "fr-par", "def-123")
	c.httpClient = srv.Client()
	c.httpClient.Transport = rewriteHost(srv.URL)

	env := map[string]string{"GITMOTE_CI_RUN_ID": "7", "WORKER_SECRET": "s3cr3t"}
	if err := c.Trigger(context.Background(), env); err != nil {
		t.Fatalf("Trigger: %v", err)
	}

	wantPath := "/serverless-jobs/v1alpha2/regions/fr-par/job-definitions/def-123/start"
	if gotPath != wantPath {
		t.Errorf("path = %q, want %q", gotPath, wantPath)
	}
	if gotToken != "secret-key" {
		t.Errorf("X-Auth-Token = %q, want secret-key", gotToken)
	}
	vars := gotBody["environment_variables"]
	if vars["GITMOTE_CI_RUN_ID"] != "7" || vars["WORKER_SECRET"] != "s3cr3t" {
		t.Errorf("environment_variables = %v, want the passed env", vars)
	}
}

func TestTriggerErrorsOnBadStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("denied"))
	}))
	defer srv.Close()

	c := NewJobsClient("k", "fr-par", "def-123")
	c.httpClient = srv.Client()
	c.httpClient.Transport = rewriteHost(srv.URL)

	err := c.Trigger(context.Background(), nil)
	if err == nil {
		t.Fatal("Trigger returned nil, want an error for a 403")
	}
	if !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "denied") {
		t.Errorf("error = %v, want it to include the status and body", err)
	}
}

func TestTriggerNoOpWhenUnconfigured(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()

	c := NewJobsClient("", "", "") // no job definition
	c.httpClient = srv.Client()
	c.httpClient.Transport = rewriteHost(srv.URL)

	if err := c.Trigger(context.Background(), map[string]string{"x": "y"}); err != nil {
		t.Fatalf("Trigger no-op returned %v, want nil", err)
	}
	if called {
		t.Error("Trigger made an HTTP call when the job definition is unset")
	}
}

// rewriteHost returns a transport that redirects every request to the test
// server's host while preserving the request path, so the client's hardcoded
// api.scaleway.com URL reaches httptest.
func rewriteHost(serverURL string) http.RoundTripper {
	base := strings.TrimPrefix(strings.TrimPrefix(serverURL, "http://"), "https://")
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		r.URL.Scheme = "http"
		r.URL.Host = base
		return http.DefaultTransport.RoundTrip(r)
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
