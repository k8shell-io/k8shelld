package logger

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"

	clogger "github.com/k8shell-io/common/pkg/logger"
	"github.com/rs/zerolog"
)

const LOGSTORE_CAPACITY = 10000

// Shared memory log store
var logStore = &MemoryLogStore{
	entries: make([]logEntry, 0, LOGSTORE_CAPACITY),
	cap:     LOGSTORE_CAPACITY,
}

// MemoryLogStore is an in-memory log store that implements io.Writer
type MemoryLogStore struct {
	mu      sync.Mutex
	entries []logEntry
	cap     int
}

// logEntry represents a single log entry
type logEntry struct {
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
	var entry logEntry

	if err := json.Unmarshal(p, &entry); err != nil {
		return 0, fmt.Errorf("failed to unmarshal log entry: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

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

// GetLogsSince returns new log entries from the given offset.
func GetLogsSince(offset int, component, level string) ([]logEntry, int) {
	logStore.mu.Lock()
	defer logStore.mu.Unlock()

	if offset >= len(logStore.entries) {
		return nil, len(logStore.entries)
	}

	if offset < 0 {
		offset = len(logStore.entries) + offset
		if offset < 0 {
			offset = 0
		}
	}

	var logs []logEntry
	for i := offset; i < len(logStore.entries); i++ {
		entry := logStore.entries[i]
		if (component == "" || entry.Component == component) && (level == "" || entry.Level == level) {
			logs = append(logs, entry)
		}
	}
	return logs, len(logStore.entries)
}
