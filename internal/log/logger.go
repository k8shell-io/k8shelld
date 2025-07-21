package log

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// JsonLogger controls whether the logger outputs in JSON format or console format.
// Set this to false to use a human-readable console format.
var JsonLogger = true

type logEntry struct {
	Timestamp string `json:"time"`      // from zerolog
	Component string `json:"component"` // extracted from JSON
	Raw       string // full original log line
}

type MemoryLogStore struct {
	mu      sync.Mutex
	entries []logEntry
	cap     int
}

var LogStore = NewMemoryLogStore(1000)

func NewMemoryLogStore(capacity int) *MemoryLogStore {
	return &MemoryLogStore{
		entries: make([]logEntry, 0, capacity),
		cap:     capacity,
	}
}

func (s *MemoryLogStore) Write(p []byte) (int, error) {
	raw := string(p)
	var entry logEntry

	if err := json.Unmarshal(p, &entry); err != nil {
		// fallback
		entry = logEntry{
			Timestamp: time.Now().Format(time.RFC3339),
			Component: "unknown",
			Raw:       raw,
		}
	} else {
		entry.Raw = raw
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.entries) >= s.cap {
		s.entries = s.entries[1:]
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

// NewLogger creates a logger writing to stdout and the shared memory log store.
func NewLogger(component string) *zerolog.Logger {
	var output io.Writer

	if !JsonLogger {
		output = zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: time.RFC3339}
	} else {
		output = io.MultiWriter(os.Stdout, LogStore)
	}

	logger := zerolog.New(output).
		With().
		Timestamp().
		Str("component", component).
		Logger()
	return &logger
}

func (s *MemoryLogStore) GetLogs(component string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	var logs []string
	for _, entry := range s.entries {
		if component == "" || entry.Component == component {
			logs = append(logs, entry.Raw)
		}
	}
	return logs
}
