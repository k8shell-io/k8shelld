// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package models

import (
	"sync"
	"time"
)

// InitTracker is a thread-safe store for the lifecycle state of all init scripts.
type InitTracker struct {
	mu      sync.RWMutex
	scripts []InitScriptState
}

// NewInitTracker returns an empty InitTracker.
func NewInitTracker() *InitTracker {
	return &InitTracker{}
}

// RegisterAll pre-populates the tracker with all script names in pending state.
// This must be called before any goroutine calls SetRunning or SetCompleted.
func (t *InitTracker) RegisterAll(names []string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.scripts = make([]InitScriptState, 0, len(names))
	for _, name := range names {
		t.scripts = append(t.scripts, InitScriptState{
			Name:   name,
			Status: InitScriptPending,
		})
	}
}

// SetRunning marks the named script as running and records its start time.
func (t *InitTracker) SetRunning(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	for i := range t.scripts {
		if t.scripts[i].Name == name {
			t.scripts[i].Status = InitScriptRunning
			t.scripts[i].StartedAt = &now
			return
		}
	}
}

// SetCompleted marks the named script as completed and records its finish time.
func (t *InitTracker) SetCompleted(name string, hasError bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	for i := range t.scripts {
		if t.scripts[i].Name == name {
			t.scripts[i].Status = InitScriptCompleted
			t.scripts[i].FinishedAt = &now
			t.scripts[i].HasError = hasError
			return
		}
	}
}

// GetAll returns a snapshot copy of all script states.
func (t *InitTracker) GetAll() []InitScriptState {
	t.mu.RLock()
	defer t.mu.RUnlock()
	result := make([]InitScriptState, len(t.scripts))
	copy(result, t.scripts)
	return result
}

// HasAny reports whether any scripts have been registered.
func (t *InitTracker) HasAny() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.scripts) > 0
}

// HasPendingOrRunning reports whether at least one script is still pending or running.
func (t *InitTracker) HasPendingOrRunning() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	for _, s := range t.scripts {
		if s.Status == InitScriptPending || s.Status == InitScriptRunning {
			return true
		}
	}
	return false
}

// AllDone reports whether every registered script has reached the completed state.
func (t *InitTracker) AllDone() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	for _, s := range t.scripts {
		if s.Status == InitScriptPending || s.Status == InitScriptRunning {
			return false
		}
	}
	return true
}
