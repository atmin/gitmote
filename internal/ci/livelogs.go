package ci

import (
	"sync"
	"time"
)

// LiveLogs holds in-memory, per-job live CI log buffers on the leader. They
// are the ephemeral tail that the UI polls while a job runs. LiveLogs is
// deliberately NOT replicated or persisted. The durable log is the blob that
// the runner ships on completion (content-before-pointer, see report.go).
//
// A leader restart loses LiveLogs. The live tail then has a short gap, but the
// durable record is unaffected. Live streaming is a best-effort UX layer, never
// a correctness path.
//
// LiveLogs is safe for concurrent use. The chunk POSTs of the runner append
// while the browser polls Read.
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

// Append adds chunk to the live buffer of jobID. It creates the buffer on the
// first write. It enforces the shared size cap with a one-time truncation
// marker, never a silent cut. Append is best-effort: it drops appends after
// the cap.
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

// Finish marks the live tail of jobID complete, so a reader knows to switch to
// the durable log. Finish does nothing for an unknown job. The buffer of that
// job may already be swept. The reader then falls back to the durable log by
// job status.
func (l *LiveLogs) Finish(jobID int64, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if lg := l.jobs[jobID]; lg != nil {
		lg.done = true
		lg.updated = now
	}
}

// Read returns three things for jobID:
//   - the buffered bytes from offset onward
//   - the next offset to poll from
//   - whether the job has finished
//
// ok is false when no live buffer exists, because the job never started or the
// buffer was swept. The caller should then fall back to the durable log.
//
// Read clamps a bad offset, so a stale client never panics:
//   - A negative offset becomes zero, so Read returns the whole buffer.
//   - An offset past the end becomes the end, so Read returns no bytes.
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

// Sweep drops buffers that are idle for longer than ttl. Idle means no Append
// and no Finish. Sweep reclaims memory once a run is done or its runner has
// died. A running job keeps its buffer fresh only while it sends log chunks.
// The runner sends no chunk for a second with no output. So a quiet running
// job can expire. A swept buffer only makes Read fall back to the durable log.
func (l *LiveLogs) Sweep(now time.Time, ttl time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for id, lg := range l.jobs {
		if now.Sub(lg.updated) > ttl {
			delete(l.jobs, id)
		}
	}
}
