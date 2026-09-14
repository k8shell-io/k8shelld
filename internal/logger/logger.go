// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package logger

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"sync"

	clogger "github.com/k8shell-io/common/pkg/logger"
	"github.com/rs/zerolog"
)

const LOGSTORE_CAPACITY = 10000

// Shared memory log store
var logStore = &MemoryLogStore{
	entries: make([]LogEntry, 0, LOGSTORE_CAPACITY),
	cap:     LOGSTORE_CAPACITY,
}

// MemoryLogStore is an in-memory log store that implements io.Writer
type MemoryLogStore struct {
	mu      sync.Mutex
	entries []LogEntry
	cap     int
	nextID  int64
}

// LogEntry represents a single log entry.
//
// ID is a per-store, monotonically increasing sequence number assigned on
// write (starting at 1). Unlike a slice index it never shifts as older
// entries are evicted from the buffer, so it's safe to hold onto as a
// pagination cursor across calls.
type LogEntry struct {
	ID        int64  `json:"id"`
	Timestamp string `json:"time"`
	Component string `json:"component"`
	Level     string `json:"level"`
	Message   string `json:"message"`
}

// NewLogger creates a logger writing to stdout and the shared memory log store.
func NewLogger(component string) *zerolog.Logger {
	writer := clogger.CreateWriter()
	multi := io.MultiWriter(writer, logStore)
	return clogger.CreateLogger(component, multi)
}

// Write implements the io.Writer interface for MemoryLogStore
func (s *MemoryLogStore) Write(p []byte) (int, error) {
	var entry LogEntry

	if err := json.Unmarshal(p, &entry); err != nil {
		return 0, fmt.Errorf("failed to unmarshal log entry: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.nextID++
	entry.ID = s.nextID

	if len(s.entries) >= s.cap {
		s.entries = s.entries[len(s.entries)-s.cap:]
	}
	s.entries = append(s.entries, entry)

	return len(p), nil
}

// InitLogLevel initializes the global log level based on the provided string.
// Valid values are "debug", "info", "warn", "error", and "fatal
func InitLogLevel(level string) error {
	switch level {
	case "debug":
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
	case "info":
		zerolog.SetGlobalLevel(zerolog.InfoLevel)
	case "warn":
		zerolog.SetGlobalLevel(zerolog.WarnLevel)
	case "error":
		zerolog.SetGlobalLevel(zerolog.ErrorLevel)
	case "fatal":
		zerolog.SetGlobalLevel(zerolog.FatalLevel)
	default:
		return fmt.Errorf("invalid log level: %s", level)
	}
	return nil
}

// GetLogsSince returns log entries with ID > sinceID (in ID order), along
// with the highest entry ID currently in the store — pass that back in as
// sinceID on the next call to continue tailing without gaps or repeats,
// regardless of how many entries have since been evicted from the buffer.
// sinceID <= 0 returns every entry currently buffered.
func GetLogsSince(sinceID int64, component, level string) ([]LogEntry, int64) {
	logStore.mu.Lock()
	defer logStore.mu.Unlock()

	lastID := sinceID
	if n := len(logStore.entries); n > 0 && logStore.entries[n-1].ID > lastID {
		lastID = logStore.entries[n-1].ID
	}

	var logs []LogEntry
	for _, entry := range logStore.entries {
		if entry.ID <= sinceID {
			continue
		}
		if (component == "" || entry.Component == component) && (level == "" || entry.Level == level) {
			logs = append(logs, entry)
		}
	}
	return logs, lastID
}

// GetLogsBefore returns up to limit log entries with ID < beforeID, oldest
// matching entry first — the "before" half of cursor-based pagination:
// pass the ID of the oldest entry from the previous page back in as
// beforeID to load the page before it. beforeID <= 0 starts from the most
// recent entry (there is no valid entry ID 0, since IDs are assigned
// starting at 1, so it doubles as the "no cursor yet" sentinel for the
// first page). The returned bool reports whether older entries remain
// unscanned in the buffer, i.e. whether a further "load more" call could
// return anything.
func GetLogsBefore(beforeID int64, limit int, component, level string) ([]LogEntry, bool) {
	logStore.mu.Lock()
	defer logStore.mu.Unlock()

	end := len(logStore.entries)
	if beforeID > 0 {
		end = sort.Search(end, func(i int) bool {
			return logStore.entries[i].ID >= beforeID
		})
	}

	var logs []LogEntry
	hasMore := false
	for i := end - 1; i >= 0; i-- {
		if len(logs) == limit {
			hasMore = true
			break
		}
		entry := logStore.entries[i]
		if (component == "" || entry.Component == component) && (level == "" || entry.Level == level) {
			logs = append(logs, entry)
		}
	}

	for i, j := 0, len(logs)-1; i < j; i, j = i+1, j-1 {
		logs[i], logs[j] = logs[j], logs[i]
	}
	return logs, hasMore
}
