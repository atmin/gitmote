package ci

import (
	"strings"
	"testing"
	"time"
)

func TestLiveLogsAppendAndRead(t *testing.T) {
	l := NewLiveLogs()
	now := time.Unix(0, 0)
	l.Append(7, []byte("hello "), now)
	l.Append(7, []byte("world"), now)

	data, next, done, ok := l.Read(7, 0)
	if !ok || done {
		t.Fatalf("Read = ok %v done %v, want ok=true done=false", ok, done)
	}
	if string(data) != "hello world" || next != 11 {
		t.Fatalf("Read = %q next %d, want %q next 11", data, next, "hello world")
	}
	// A follow-up read from the last offset returns only the new bytes.
	l.Append(7, []byte("!"), now)
	data, next, _, _ = l.Read(7, next)
	if string(data) != "!" || next != 12 {
		t.Fatalf("incremental Read = %q next %d, want %q next 12", data, next, "!")
	}
}

func TestLiveLogsReadUnknownJob(t *testing.T) {
	// The failure path: no buffer for the job → ok=false so the caller falls back
	// to the durable log rather than showing an empty live tail forever.
	data, next, done, ok := NewLiveLogs().Read(99, 3)
	if ok || done || data != nil || next != 3 {
		t.Fatalf("Read unknown = data %q next %d done %v ok %v, want nil/3/false/false", data, next, done, ok)
	}
}

func TestLiveLogsReadClampsOffset(t *testing.T) {
	l := NewLiveLogs()
	l.Append(1, []byte("abc"), time.Unix(0, 0))
	// An over-long offset (stale client) clamps to the end, never panics.
	if data, next, _, ok := l.Read(1, 999); !ok || len(data) != 0 || next != 3 {
		t.Fatalf("clamped Read = %q next %d ok %v, want empty/3/true", data, next, ok)
	}
	// A negative offset clamps to 0.
	if data, _, _, _ := l.Read(1, -5); string(data) != "abc" {
		t.Fatalf("negative-offset Read = %q, want abc", data)
	}
}

func TestLiveLogsFinish(t *testing.T) {
	l := NewLiveLogs()
	now := time.Unix(0, 0)
	l.Append(2, []byte("x"), now)
	if _, _, done, _ := l.Read(2, 0); done {
		t.Fatal("job reported done before Finish")
	}
	l.Finish(2, now)
	if _, _, done, ok := l.Read(2, 0); !ok || !done {
		t.Fatalf("after Finish: ok %v done %v, want both true", ok, done)
	}
	l.Finish(404, now) // unknown job is a no-op, not a panic
}

func TestLiveLogsCapTruncatesOnce(t *testing.T) {
	l := &LiveLogs{jobs: map[int64]*liveLog{}, cap: 8}
	now := time.Unix(0, 0)
	l.Append(1, []byte("0123456"), now) // 7 bytes, under cap 8
	l.Append(1, []byte("789abc"), now)  // crosses the cap
	l.Append(1, []byte("more"), now)    // dropped (already truncated)

	data, _, _, _ := l.Read(1, 0)
	if !strings.HasPrefix(string(data), "01234567") {
		t.Fatalf("data = %q, want the first 8 bytes kept", data)
	}
	if !strings.Contains(string(data), logTruncationMarker) {
		t.Errorf("data = %q, want an explicit truncation marker", data)
	}
	if strings.Contains(string(data), "more") {
		t.Errorf("data = %q, want post-cap appends dropped", data)
	}
}

func TestLiveLogsSweep(t *testing.T) {
	l := NewLiveLogs()
	start := time.Unix(1000, 0)
	l.Append(1, []byte("old"), start)
	l.Append(2, []byte("fresh"), start.Add(9*time.Minute))

	// Sweep at start+10m with a 5m TTL: job 1 (idle 10m) goes, job 2 (idle 1m) stays.
	l.Sweep(start.Add(10*time.Minute), 5*time.Minute)
	if _, _, _, ok := l.Read(1, 0); ok {
		t.Error("job 1 should have been swept (idle past TTL)")
	}
	if _, _, _, ok := l.Read(2, 0); !ok {
		t.Error("job 2 should survive (idle within TTL)")
	}
}
