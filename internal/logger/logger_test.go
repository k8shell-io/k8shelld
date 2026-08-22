// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package logger

import (
	"encoding/json"
	"testing"

	"github.com/rs/zerolog"
)

func TestMemoryLogStore_Initialization(t *testing.T) {
	// Test that the package-level logStore is initialized properly
	if logStore == nil {
		t.Fatal("logStore is nil")
	}

	if logStore.cap != LOGSTORE_CAPACITY {
		t.Errorf("expected capacity %d, got %d", LOGSTORE_CAPACITY, logStore.cap)
	}
}

func TestMemoryLogStore_Write(t *testing.T) {
	// Create a test store
	store := &MemoryLogStore{
		entries: make([]LogEntry, 0, 10),
		cap:     10,
	}

	logData := LogEntry{
		Timestamp: "2025-12-18T10:00:00Z",
		Component: "test-component",
		Level:     "info",
		Message:   "test message",
	}

	data, err := json.Marshal(logData)
	if err != nil {
		t.Fatalf("failed to marshal log entry: %v", err)
	}

	n, err := store.Write(data)
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	if n != len(data) {
		t.Errorf("expected to write %d bytes, wrote %d", len(data), n)
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	if len(store.entries) != 1 {
		t.Errorf("expected 1 entry, got %d", len(store.entries))
	}

	if store.entries[0].Component != "test-component" {
		t.Errorf("expected component 'test-component', got '%s'", store.entries[0].Component)
	}
}

func TestMemoryLogStore_Write_InvalidJSON(t *testing.T) {
	store := &MemoryLogStore{
		entries: make([]LogEntry, 0, 10),
		cap:     10,
	}

	invalidData := []byte("not valid json")
	_, err := store.Write(invalidData)
	if err == nil {
		t.Error("expected error for invalid JSON, got nil")
	}
}

func TestMemoryLogStore_CapacityLimit(t *testing.T) {
	capacity := 5
	store := &MemoryLogStore{
		entries: make([]LogEntry, 0, capacity),
		cap:     capacity,
	}

	// Write more entries than capacity
	for i := 0; i < 10; i++ {
		logData := LogEntry{
			Timestamp: "2025-12-18T10:00:00Z",
			Component: "test",
			Level:     "info",
			Message:   string(rune('A' + i)),
		}

		data, _ := json.Marshal(logData)
		_, err := store.Write(data)
		if err != nil {
			t.Fatalf("Write failed at iteration %d: %v", i, err)
		}
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	// With the logic: if len >= cap, then s.entries[len-cap:] is taken before appending.
	// This means that after appending, the slice length will be cap+1.
	// After writing 10 entries with capacity 5:
	// - When writing entry 6 (F): len=5, slices to [0:], appends F -> len=6
	// - When writing entry 7 (G): len=6, slices to [1:], appends G -> len=6
	// - etc.
	// Final entries slice should have length capacity+1 (6 entries, indices [0]..[5]):
	//   messages E, F, G, H, I, J. This assertion validates the capacity+1 behavior.
	expectedLen := capacity + 1
	if len(store.entries) != expectedLen {
		t.Errorf("expected %d entries, got %d", expectedLen, len(store.entries))
	}

	if store.entries[0].Message != "E" {
		t.Errorf("expected first entry to be 'E', got '%s'", store.entries[0].Message)
	}

	if store.entries[5].Message != "J" {
		t.Errorf("expected last entry to be 'J', got '%s'", store.entries[5].Message)
	}
}

func TestGetLogsSince_Basic(t *testing.T) {
	// Save and restore original logStore
	originalStore := logStore
	defer func() { logStore = originalStore }()

	// Create test store
	logStore = &MemoryLogStore{
		entries: make([]LogEntry, 0, 10),
		cap:     10,
	}

	// Add some test entries
	entries := []LogEntry{
		{Timestamp: "2025-12-18T10:00:00Z", Component: "comp1", Level: "info", Message: "msg1"},
		{Timestamp: "2025-12-18T10:01:00Z", Component: "comp2", Level: "debug", Message: "msg2"},
		{Timestamp: "2025-12-18T10:02:00Z", Component: "comp1", Level: "error", Message: "msg3"},
	}

	for _, entry := range entries {
		data, _ := json.Marshal(entry)
		_, err := logStore.Write(data)
		if err != nil {
			t.Fatalf("failed to write log entry: %v", err)
		}
	}

	logs, newOffset := GetLogsSince(0, "", "")

	if len(logs) != 3 {
		t.Errorf("expected 3 logs, got %d", len(logs))
	}

	if newOffset != 3 {
		t.Errorf("expected newOffset 3, got %d", newOffset)
	}
}

func TestGetLogsSince_WithOffset(t *testing.T) {
	// Save and restore original logStore
	originalStore := logStore
	defer func() { logStore = originalStore }()

	// Create test store
	logStore = &MemoryLogStore{
		entries: make([]LogEntry, 0, 10),
		cap:     10,
	}

	// Add test entries
	for i := 0; i < 5; i++ {
		entry := LogEntry{
			Timestamp: "2025-12-18T10:00:00Z",
			Component: "test",
			Level:     "info",
			Message:   string(rune('A' + i)),
		}
		data, _ := json.Marshal(entry)
		_, err := logStore.Write(data)
		if err != nil {
			t.Fatalf("failed to write log entry: %v", err)
		}
	}

	logs, newOffset := GetLogsSince(2, "", "")

	if len(logs) != 3 {
		t.Errorf("expected 3 logs from offset 2, got %d", len(logs))
	}

	if newOffset != 5 {
		t.Errorf("expected newOffset 5, got %d", newOffset)
	}

	if logs[0].Message != "C" {
		t.Errorf("expected first log message 'C', got '%s'", logs[0].Message)
	}
}

func TestGetLogsBefore_MostRecent(t *testing.T) {
	// Save and restore original logStore
	originalStore := logStore
	defer func() { logStore = originalStore }()

	// Create test store
	logStore = &MemoryLogStore{
		entries: make([]LogEntry, 0, 10),
		cap:     10,
	}

	// Add test entries
	for i := 0; i < 5; i++ {
		entry := LogEntry{
			Timestamp: "2025-12-18T10:00:00Z",
			Component: "test",
			Level:     "info",
			Message:   string(rune('A' + i)),
		}
		data, _ := json.Marshal(entry)
		_, err := logStore.Write(data)
		if err != nil {
			t.Fatalf("failed to write log entry: %v", err)
		}
	}

	// beforeID <= 0 should get the last 2 entries, oldest-first
	logs, hasMore := GetLogsBefore(0, 2, "", "")

	if len(logs) != 2 {
		t.Fatalf("expected 2 logs, got %d", len(logs))
	}

	if !hasMore {
		t.Error("expected hasMore=true, since 3 older entries remain")
	}

	if logs[0].Message != "D" || logs[1].Message != "E" {
		t.Errorf("expected [D, E], got [%s, %s]", logs[0].Message, logs[1].Message)
	}
}

func TestGetLogsBefore_Pagination(t *testing.T) {
	// Save and restore original logStore
	originalStore := logStore
	defer func() { logStore = originalStore }()

	// Create test store
	logStore = &MemoryLogStore{
		entries: make([]LogEntry, 0, 10),
		cap:     10,
	}

	// Add 5 entries: A..E, IDs 1..5
	for i := 0; i < 5; i++ {
		entry := LogEntry{
			Timestamp: "2025-12-18T10:00:00Z",
			Component: "test",
			Level:     "info",
			Message:   string(rune('A' + i)),
		}
		data, _ := json.Marshal(entry)
		_, err := logStore.Write(data)
		if err != nil {
			t.Fatalf("failed to write log entry: %v", err)
		}
	}

	// Page 1: most recent 2 -> [D, E], more remain (A, B, C)
	page1, hasMore1 := GetLogsBefore(0, 2, "", "")
	if len(page1) != 2 || page1[0].Message != "D" || page1[1].Message != "E" || !hasMore1 {
		t.Fatalf("unexpected page1: %+v hasMore=%v", page1, hasMore1)
	}

	// Page 2: before the oldest entry of page1 -> [B, C], A remains
	page2, hasMore2 := GetLogsBefore(page1[0].ID, 2, "", "")
	if len(page2) != 2 || page2[0].Message != "B" || page2[1].Message != "C" || !hasMore2 {
		t.Fatalf("unexpected page2: %+v hasMore=%v", page2, hasMore2)
	}

	// Page 3: before the oldest entry of page2 -> [A], nothing left
	page3, hasMore3 := GetLogsBefore(page2[0].ID, 2, "", "")
	if len(page3) != 1 || page3[0].Message != "A" || hasMore3 {
		t.Fatalf("unexpected page3: %+v hasMore=%v", page3, hasMore3)
	}
}

// TestGetLogsBefore_StableAcrossEviction verifies that a cursor obtained
// before older entries are evicted from the buffer still lands in the
// right place afterwards — the whole point of using a stable sequence id
// instead of a slice index, which would shift under eviction.
func TestGetLogsBefore_StableAcrossEviction(t *testing.T) {
	// Save and restore original logStore
	originalStore := logStore
	defer func() { logStore = originalStore }()

	capacity := 5
	logStore = &MemoryLogStore{
		entries: make([]LogEntry, 0, capacity),
		cap:     capacity,
	}

	// Write 10 entries (IDs 1..10); with capacity 5 the store retains the
	// last 6 (see TestMemoryLogStore_CapacityLimit), i.e. messages E..J
	// with IDs 5..10.
	for i := 0; i < 10; i++ {
		entry := LogEntry{
			Timestamp: "2025-12-18T10:00:00Z",
			Component: "test",
			Level:     "info",
			Message:   string(rune('A' + i)),
		}
		data, _ := json.Marshal(entry)
		if _, err := logStore.Write(data); err != nil {
			t.Fatalf("Write failed at iteration %d: %v", i, err)
		}
	}

	// Cursor id 8 ("H") predates eviction of "A"-"D"; paging before it must
	// still resolve correctly against what remains in the buffer: entries
	// with ID < 8 that are still present are E(5), F(6), G(7).
	logs, hasMore := GetLogsBefore(8, 10, "", "")
	if len(logs) != 3 || logs[0].Message != "E" || logs[1].Message != "F" || logs[2].Message != "G" {
		t.Fatalf("expected [E, F, G], got %+v", logs)
	}
	if hasMore {
		t.Error("expected hasMore=false, buffer has no entries older than E")
	}
}

func TestGetLogsSince_ComponentFilter(t *testing.T) {
	// Save and restore original logStore
	originalStore := logStore
	defer func() { logStore = originalStore }()

	// Create test store
	logStore = &MemoryLogStore{
		entries: make([]LogEntry, 0, 10),
		cap:     10,
	}

	entries := []LogEntry{
		{Timestamp: "2025-12-18T10:00:00Z", Component: "comp1", Level: "info", Message: "msg1"},
		{Timestamp: "2025-12-18T10:01:00Z", Component: "comp2", Level: "info", Message: "msg2"},
		{Timestamp: "2025-12-18T10:02:00Z", Component: "comp1", Level: "info", Message: "msg3"},
		{Timestamp: "2025-12-18T10:03:00Z", Component: "comp3", Level: "info", Message: "msg4"},
	}

	for _, entry := range entries {
		data, _ := json.Marshal(entry)
		_, err := logStore.Write(data)
		if err != nil {
			t.Fatalf("failed to write log entry: %v", err)
		}
	}

	logs, newOffset := GetLogsSince(0, "comp1", "")

	if len(logs) != 2 {
		t.Errorf("expected 2 logs for component 'comp1', got %d", len(logs))
	}

	if newOffset != 4 {
		t.Errorf("expected newOffset 4, got %d", newOffset)
	}

	for _, log := range logs {
		if log.Component != "comp1" {
			t.Errorf("expected component 'comp1', got '%s'", log.Component)
		}
	}
}

func TestGetLogsSince_LevelFilter(t *testing.T) {
	// Save and restore original logStore
	originalStore := logStore
	defer func() { logStore = originalStore }()

	// Create test store
	logStore = &MemoryLogStore{
		entries: make([]LogEntry, 0, 10),
		cap:     10,
	}

	entries := []LogEntry{
		{Timestamp: "2025-12-18T10:00:00Z", Component: "comp1", Level: "info", Message: "msg1"},
		{Timestamp: "2025-12-18T10:01:00Z", Component: "comp1", Level: "error", Message: "msg2"},
		{Timestamp: "2025-12-18T10:02:00Z", Component: "comp1", Level: "info", Message: "msg3"},
		{Timestamp: "2025-12-18T10:03:00Z", Component: "comp1", Level: "debug", Message: "msg4"},
	}

	for _, entry := range entries {
		data, _ := json.Marshal(entry)
		_, err := logStore.Write(data)
		if err != nil {
			t.Fatalf("failed to write log entry: %v", err)
		}
	}

	logs, newOffset := GetLogsSince(0, "", "error")

	if len(logs) != 1 {
		t.Errorf("expected 1 log with level 'error', got %d", len(logs))
	}

	if newOffset != 4 {
		t.Errorf("expected newOffset 4, got %d", newOffset)
	}

	if logs[0].Level != "error" {
		t.Errorf("expected level 'error', got '%s'", logs[0].Level)
	}
}

func TestGetLogsSince_CombinedFilters(t *testing.T) {
	// Save and restore original logStore
	originalStore := logStore
	defer func() { logStore = originalStore }()

	// Create test store
	logStore = &MemoryLogStore{
		entries: make([]LogEntry, 0, 10),
		cap:     10,
	}

	entries := []LogEntry{
		{Timestamp: "2025-12-18T10:00:00Z", Component: "comp1", Level: "info", Message: "msg1"},
		{Timestamp: "2025-12-18T10:01:00Z", Component: "comp2", Level: "error", Message: "msg2"},
		{Timestamp: "2025-12-18T10:02:00Z", Component: "comp1", Level: "error", Message: "msg3"},
		{Timestamp: "2025-12-18T10:03:00Z", Component: "comp1", Level: "debug", Message: "msg4"},
	}

	for _, entry := range entries {
		data, _ := json.Marshal(entry)
		_, err := logStore.Write(data)
		if err != nil {
			t.Fatalf("failed to write log entry: %v", err)
		}
	}

	logs, _ := GetLogsSince(0, "comp1", "error")

	if len(logs) != 1 {
		t.Errorf("expected 1 log with component 'comp1' and level 'error', got %d", len(logs))
	}

	if logs[0].Message != "msg3" {
		t.Errorf("expected message 'msg3', got '%s'", logs[0].Message)
	}
}

func TestGetLogsSince_SinceIDBeyondLatest(t *testing.T) {
	// Save and restore original logStore
	originalStore := logStore
	defer func() { logStore = originalStore }()

	// Create test store
	logStore = &MemoryLogStore{
		entries: make([]LogEntry, 0, 10),
		cap:     10,
	}

	// Add 3 entries (IDs 1..3)
	for i := 0; i < 3; i++ {
		entry := LogEntry{
			Timestamp: "2025-12-18T10:00:00Z",
			Component: "test",
			Level:     "info",
			Message:   string(rune('A' + i)),
		}
		data, _ := json.Marshal(entry)
		_, err := logStore.Write(data)
		if err != nil {
			t.Fatalf("failed to write log entry: %v", err)
		}
	}

	// A sinceID beyond the latest entry id has nothing new to return, and
	// the cursor doesn't move backward.
	logs, lastID := GetLogsSince(10, "", "")

	if logs != nil {
		t.Errorf("expected nil logs for sinceID beyond the latest entry, got %d logs", len(logs))
	}

	if lastID != 10 {
		t.Errorf("expected lastID to stay at 10, got %d", lastID)
	}
}

func TestInitLogLevel_ValidLevels(t *testing.T) {
	tests := []struct {
		level    string
		expected zerolog.Level
	}{
		{"debug", zerolog.DebugLevel},
		{"info", zerolog.InfoLevel},
		{"warn", zerolog.WarnLevel},
		{"error", zerolog.ErrorLevel},
		{"fatal", zerolog.FatalLevel},
	}

	for _, tt := range tests {
		t.Run(tt.level, func(t *testing.T) {
			err := InitLogLevel(tt.level)
			if err != nil {
				t.Errorf("InitLogLevel(%s) returned error: %v", tt.level, err)
			}

			if zerolog.GlobalLevel() != tt.expected {
				t.Errorf("expected global level %v, got %v", tt.expected, zerolog.GlobalLevel())
			}
		})
	}
}

func TestInitLogLevel_InvalidLevel(t *testing.T) {
	err := InitLogLevel("invalid-level")
	if err == nil {
		t.Error("expected error for invalid log level, got nil")
	}
}

func TestNewLogger(t *testing.T) {
	component := "test-component"

	logger := NewLogger(component)

	if logger == nil {
		t.Fatal("NewLogger returned nil")
	}

	// The logger should be usable
	logger.Info().Msg("test message")
}
