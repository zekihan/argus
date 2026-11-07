package state

import (
	"context"
	"strconv"
	"sync"

	"github.com/zekihan/argus/internal/kube"
)

// Memory is an in-process Backend for tests and offline evaluation. Hooks let
// tests inject conflicts, timeouts and concurrent writers.
type Memory struct {
	mu      sync.Mutex
	data    string
	present bool
	version int
	missing bool
	writes  int
	calls   int

	// BeforeWrite runs before a write is applied, with the zero-based count of
	// earlier Write calls; a non-nil error fails it.
	BeforeWrite func(attempt int) error
	// AfterWrite runs after a write is applied; a non-nil error is returned
	// even though the write landed, like a timeout after the server committed.
	AfterWrite func(attempt int) error
	// ReadError, when set, fails reads.
	ReadError func() error
}

// NewMemory holds data under Key; present=false simulates a missing key.
func NewMemory(data string, present bool) *Memory {
	return &Memory{data: data, present: present, version: 1}
}

// Missing simulates an absent ConfigMap.
func (m *Memory) Missing() *Memory { m.missing = true; return m }

// Set replaces the stored document as another writer would.
func (m *Memory) Set(data string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data, m.present = data, true
	m.version++
}

// Data returns the stored document.
func (m *Memory) Data() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.data
}

// Writes counts applied writes.
func (m *Memory) Writes() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.writes
}

func (m *Memory) Read(context.Context) (string, bool, string, error) {
	if m.ReadError != nil {
		if err := m.ReadError(); err != nil {
			return "", false, "", err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.missing {
		return "", false, "", ErrMissing
	}
	return m.data, m.present, strconv.Itoa(m.version), nil
}

func (m *Memory) Write(_ context.Context, data, version string) (string, error) {
	m.mu.Lock()
	attempt := m.calls
	m.calls++
	m.mu.Unlock()
	if m.BeforeWrite != nil {
		if err := m.BeforeWrite(attempt); err != nil {
			return "", err
		}
	}
	m.mu.Lock()
	if version != strconv.Itoa(m.version) {
		m.mu.Unlock()
		return "", errConflict
	}
	m.data, m.present = data, true
	m.version++
	m.writes++
	next := strconv.Itoa(m.version)
	m.mu.Unlock()
	if m.AfterWrite != nil {
		if err := m.AfterWrite(attempt); err != nil {
			return "", err
		}
	}
	return next, nil
}

var errConflict = &kube.Error{Code: kube.CodeConflict, Status: 409, Path: "memory"}
