package ci

import (
	"sync"
	"time"
)

// LiveLogs holds in-memory, per-job live CI log buffers on the leader — the
// ephemeral tail the UI polls while a job runs. It is deliberately NOT replicated
// or persisted: the durable log is the blob the runner ships on completion
// (content-before-pointer, see report.go). If this is lost (a leader restart), the
// live tail blips but the durable record is unaffected — live streaming is a
// best-effort UX layer, never a correctness path. Safe for concurrent use: the
// runner's chunk POSTs append while the browser polls Read.
type LiveLogs struct {
	mu   sync.Mutex
	jobs map[int64]*liveLog
	cap  int
}

type liveLog struct {
	buf       []byte
	done      bool
	truncated bool
	updated   time.Time
}

// NewLiveLogs returns an empty store capped at the same size as the durable log.
func NewLiveLogs() *LiveLogs {
	return &LiveLogs{jobs: map[int64]*liveLog{}, cap: logCap}
}

// Append adds chunk to jobID's live buffer, creating it on first write and
// enforcing the shared size cap with a one-time truncation marker (never a silent
// cut). Best-effort: appends after the cap are dropped.
func (l *LiveLogs) Append(jobID int64, chunk []byte, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	lg := l.jobs[jobID]
	if lg == nil {
		lg = &liveLog{}
		l.jobs[jobID] = lg
	}
	lg.updated = now
	if lg.truncated {
		return
	}
	if room := l.cap - len(lg.buf); len(chunk) > room {
		if room > 0 {
			lg.buf = append(lg.buf, chunk[:room]...)
		}
		lg.buf = append(lg.buf, logTruncationMarker...)
		lg.truncated = true
		return
	}
	lg.buf = append(lg.buf, chunk...)
}

// Finish marks jobID's live tail complete so a reader knows to switch to the
// durable log. A no-op for an unknown job (its buffer may already be swept — the
// reader then falls back to the durable log by job status).
func (l *LiveLogs) Finish(jobID int64, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if lg := l.jobs[jobID]; lg != nil {
		lg.done = true
		lg.updated = now
	}
}

// Read returns jobID's buffered bytes from offset onward, the next offset to poll
// from, and whether the job has finished. ok is false when no live buffer exists
// (never started, or swept) — the caller should fall back to the durable log. A
// negative or over-long offset is clamped, so a stale client never panics or
// re-reads the whole buffer.
func (l *LiveLogs) Read(jobID int64, offset int) (data []byte, next int, done, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	lg := l.jobs[jobID]
	if lg == nil {
		return nil, offset, false, false
	}
	switch {
	case offset < 0:
		offset = 0
	case offset > len(lg.buf):
		offset = len(lg.buf)
	}
	out := make([]byte, len(lg.buf)-offset)
	copy(out, lg.buf[offset:])
	return out, len(lg.buf), lg.done, true
}

// Sweep drops buffers idle (no append/finish) for longer than ttl, reclaiming
// memory once a run is done or its runner has died. Active jobs flush every second
// so their buffers stay fresh; a swept buffer just makes Read fall back to the
// durable log.
func (l *LiveLogs) Sweep(now time.Time, ttl time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for id, lg := range l.jobs {
		if now.Sub(lg.updated) > ttl {
			delete(l.jobs, id)
		}
	}
}
