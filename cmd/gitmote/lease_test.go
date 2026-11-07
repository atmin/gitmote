package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/atmin/s3lite"
)

// idleFor returns an activity whose last request ended d ago.
func idleFor(d time.Duration) *leaseActivity {
	a := newLeaseActivity()
	a.last.Store(time.Now().Add(-d).UnixNano())
	return a
}

func TestMaybeYield(t *testing.T) {
	// An idle writer hands the lease back. These keep the lease as it is:
	//   - a busy writer
	//   - a follower
	//   - a writer with a request in flight
	//
	// A failed yield reports false. Only an unexpected error is logged.
	// ErrNotLeader means that there was nothing to yield.
	errBoom := errors.New("final sync failed")
	cases := []struct {
		name      string
		leader    bool
		idle      time.Duration
		inFlight  bool
		yieldErr  error
		want      bool
		wantCalls int
		wantWarn  bool
	}{
		{name: "idle writer yields", leader: true, idle: idleYieldAfter + time.Second, want: true, wantCalls: 1},
		{name: "recently busy writer keeps the lease", leader: true, idle: time.Second},
		{name: "follower has nothing to yield", leader: false, idle: time.Hour},
		{name: "request in flight blocks the yield", leader: true, idle: time.Hour, inFlight: true},
		{name: "failed yield is logged", leader: true, idle: time.Hour, yieldErr: errBoom, wantCalls: 1, wantWarn: true},
		{name: "lost lease is quiet", leader: true, idle: time.Hour, yieldErr: s3lite.ErrNotLeader, wantCalls: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logs, nil))
			a := idleFor(tc.idle)
			if tc.inFlight {
				a.mu.RLock()
				defer a.mu.RUnlock()
			}
			calls := 0
			yield := func(context.Context) error { calls++; return tc.yieldErr }

			got := a.maybeYield(context.Background(), func() bool { return tc.leader }, yield, logger)
			if got != tc.want {
				t.Errorf("maybeYield = %v, want %v", got, tc.want)
			}
			if calls != tc.wantCalls {
				t.Errorf("yield called %d times, want %d", calls, tc.wantCalls)
			}
			if warned := strings.Contains(logs.String(), "level=WARN"); warned != tc.wantWarn {
				t.Errorf("warned = %v, want %v; logs: %s", warned, tc.wantWarn, logs.String())
			}
		})
	}
}

func TestTrackCountsRequestsButNotProbes(t *testing.T) {
	// Probes and static assets must not reset the idle clock. Otherwise, a health
	// check every few seconds would keep the lease on this instance forever.
	// Every other request does reset the idle clock.
	a := idleFor(time.Hour)
	srv := httptest.NewServer(a.track(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})))
	defer srv.Close()
	get := func(path string) {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
	}

	for _, p := range []string{"/healthz", "/version", "/static/app.css"} {
		get(p)
		if !a.idle() {
			t.Fatalf("GET %s counted as activity; want probes and assets ignored", p)
		}
	}
	get("/some/repo/info/refs")
	if a.idle() {
		t.Error("a git request did not count as activity")
	}
}

func TestNoYieldWhileARequestRuns(t *testing.T) {
	// A request holds the activity read lock for its whole life. So a yield can
	// never fence a request in flight, even one that has outlasted idleYieldAfter.
	a := idleFor(time.Hour)
	entered, release := make(chan struct{}), make(chan struct{})
	h := a.track(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(entered)
		<-release
	}))
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/some/repo/info/refs", nil))
	}()
	<-entered

	leader := func() bool { return true }
	yield := func(context.Context) error { return nil }
	logger := slog.New(slog.DiscardHandler)
	if a.maybeYield(context.Background(), leader, yield, logger) {
		t.Fatal("yielded while a request was in flight")
	}
	close(release)
	<-done

	// The request just ended, so the writer is not idle yet.
	if a.maybeYield(context.Background(), leader, yield, logger) {
		t.Fatal("yielded right after a request ended; want idleYieldAfter first")
	}
	a.last.Store(time.Now().Add(-idleYieldAfter).UnixNano())
	if !a.maybeYield(context.Background(), leader, yield, logger) {
		t.Error("did not yield once idle with nothing in flight")
	}
}

func TestRequestDuringYieldWaits(t *testing.T) {
	// A request that arrives during a yield waits for the yield to finish. So the
	// gate checks it against the state after the yield. It is never served during
	// the yield.
	a := idleFor(time.Hour)
	inYield, finishYield := make(chan struct{}), make(chan struct{})
	yielded := false
	yield := func(context.Context) error {
		close(inYield)
		<-finishYield
		yielded = true
		return nil
	}
	go a.maybeYield(context.Background(), func() bool { return true }, yield, slog.New(slog.DiscardHandler))
	<-inYield

	served := make(chan bool, 1)
	h := a.track(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		served <- yielded
	}))
	go h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/some/repo/info/refs", nil))

	select {
	case <-served:
		t.Fatal("request served while the yield was running")
	case <-time.After(100 * time.Millisecond):
	}
	close(finishYield)
	select {
	case afterYield := <-served:
		if !afterYield {
			t.Error("request served before the yield completed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("request never served after the yield")
	}
}

func TestReconcileOnce(t *testing.T) {
	// The sweep runs only as the writer. It takes the lease through the gate, like
	// a request does. It always runs under the activity read lock, so it never
	// overlaps a yield.
	cases := []struct {
		name      string
		gate      func(context.Context) bool
		wantSwept bool
	}{
		{name: "lease taken: sweeps", gate: func(context.Context) bool { return true }, wantSwept: true},
		{name: "lease held elsewhere: skips", gate: func(context.Context) bool { return false }},
		{name: "no replica: sweeps", gate: nil, wantSwept: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := idleFor(time.Hour)
			swept := false
			sweep := func(context.Context) error {
				swept = true
				if a.mu.TryLock() {
					a.mu.Unlock()
					t.Error("sweep ran without the activity read lock")
				}
				return nil
			}
			if err := reconcileOnce(context.Background(), a, tc.gate, sweep); err != nil {
				t.Fatalf("reconcileOnce: %v", err)
			}
			if swept != tc.wantSwept {
				t.Errorf("swept = %v, want %v", swept, tc.wantSwept)
			}
			if !a.idle() {
				t.Error("a reconcile tick counted as activity; want the lease free on the next check")
			}
		})
	}
}
