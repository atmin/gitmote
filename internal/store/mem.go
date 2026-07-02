package store

import (
	"bytes"
	"context"
	"io"
	"sort"
	"strings"
	"sync"
)

// Mem is an in-memory Store for tests. It must stay in lockstep with the S3
// implementation — the conformance suite runs against both.
type Mem struct {
	mu      sync.RWMutex
	objects map[string][]byte
}

// NewMem returns an empty in-memory store.
func NewMem() *Mem {
	return &Mem{objects: make(map[string][]byte)}
}

var _ Store = (*Mem)(nil)

// Put implements Store.
func (m *Mem) Put(ctx context.Context, key string, r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = data
	return nil
}

// Get implements Store.
func (m *Mem) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	data, ok := m.objects[key]
	if !ok {
		return nil, ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

// Exists implements Store.
func (m *Mem) Exists(ctx context.Context, key string) (bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.objects[key]
	return ok, nil
}

// List implements Store.
func (m *Mem) List(ctx context.Context, prefix string) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var keys []string
	for k := range m.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys, nil
}
