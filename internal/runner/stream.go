package runner

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// liveLogFlushInterval is how often the streamer ships accumulated output to the
// leader's live buffer. Short enough to feel live, long enough to keep the HTTP
// overhead negligible against a multi-minute build.
const liveLogFlushInterval = time.Second

// logStreamer is the io.Writer the engine tees its combined output to. A
// background goroutine periodically POSTs the accumulated bytes to the leader's
// live-log endpoint, so the UI can tail a running job. It is deliberately
// best-effort: a failed POST drops that chunk (the durable log is shipped whole at
// completion), so streaming never blocks or fails the run.
type logStreamer struct {
	url    string
	secret string
	client *http.Client
	logger *slog.Logger

	mu      sync.Mutex
	pending []byte

	stopCh chan struct{}
	doneCh chan struct{}
}

// newLogStreamer builds a streamer targeting cfg's leader for jobID.
func newLogStreamer(cfg Config, jobID int64) *logStreamer {
	return &logStreamer{
		url:    fmt.Sprintf("%s/internal/ci/jobs/%d/log", cfg.BaseURL, jobID),
		secret: cfg.WorkerSecret,
		client: cfg.HTTPClient,
		logger: cfg.Logger,
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}
}

// Write buffers p for the next flush. It never errors, so teeing it into the
// engine's output can't disturb the real capture.
func (s *logStreamer) Write(p []byte) (int, error) {
	s.mu.Lock()
	s.pending = append(s.pending, p...)
	s.mu.Unlock()
	return len(p), nil
}

// start launches the flush loop. Call stop exactly once to end it.
func (s *logStreamer) start() { go s.loop() }

// stop ends the flush loop after a final flush, and blocks until it returns.
func (s *logStreamer) stop() {
	close(s.stopCh)
	<-s.doneCh
}

func (s *logStreamer) loop() {
	defer close(s.doneCh)
	t := time.NewTicker(liveLogFlushInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			s.flush()
		case <-s.stopCh:
			s.flush() // ship whatever's left before the durable completion
			return
		}
	}
}

// flush POSTs the pending bytes and clears them. On failure the chunk is dropped
// (best-effort): the durable log still carries everything, so a live gap is
// cosmetic.
func (s *logStreamer) flush() {
	s.mu.Lock()
	chunk := s.pending
	s.pending = nil
	s.mu.Unlock()
	if len(chunk) == 0 {
		return
	}
	if err := s.post(chunk); err != nil {
		s.logger.Debug("ci runner: live log flush dropped", "error", err)
	}
}

func (s *logStreamer) post(chunk []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(chunk))
	if err != nil {
		return err
	}
	req.Header.Set("X-Worker-Secret", s.secret)
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("live log chunk: status %d", resp.StatusCode)
	}
	return nil
}
