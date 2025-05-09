package server

import (
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/sirupsen/logrus/hooks/test"
)

func TestNewLogger(t *testing.T) {
	// Arrange
	component := "test-component"

	// Act
	logger := NewLogger(component)

	// Assert
	if logger.component != component {
		t.Errorf("expected component %s, got %s", component, logger.component)
	}
	if logger.logger.Level != logrus.DebugLevel {
		t.Errorf("expected log level %v, got %v", logrus.DebugLevel, logger.logger.Level)
	}
}

func TestLoggerInfo(t *testing.T) {
	// Arrange
	logger := NewLogger("test-component")
	hook := test.NewLocal(logger.logger)
	defer hook.Reset()

	// Act
	logger.Info("Test info log")

	// Assert
	if len(hook.Entries) != 1 {
		t.Fatalf("expected 1 log entry, got %d", len(hook.Entries))
	}

	entry := hook.Entries[0]
	if entry.Message != "Test info log" {
		t.Errorf("unexpected message: %v", entry.Message)
	}
	if entry.Level != logrus.InfoLevel {
		t.Errorf("unexpected log level: %v", entry.Level)
	}
	if entry.Data["component"] != "test-component" {
		t.Errorf("unexpected component field: %v", entry.Data["component"])
	}
}

func TestLoggerError(t *testing.T) {
	// Arrange
	logger := NewLogger("test-component")
	hook := test.NewLocal(logger.logger)
	defer hook.Reset()

	// Act
	logger.Error("Test error log")

	// Assert
	if len(hook.Entries) != 1 {
		t.Fatalf("expected 1 log entry, got %d", len(hook.Entries))
	}

	entry := hook.Entries[0]
	if entry.Message != "Test error log" {
		t.Errorf("unexpected message: %v", entry.Message)
	}
	if entry.Level != logrus.ErrorLevel {
		t.Errorf("unexpected log level: %v", entry.Level)
	}
	if entry.Data["component"] != "test-component" {
		t.Errorf("unexpected component field: %v", entry.Data["component"])
	}
}

func TestLoggerErrorR(t *testing.T) {
	// Arrange
	logger := NewLogger("test-component")
	hook := test.NewLocal(logger.logger)
	defer hook.Reset()

	// Act
	err := logger.ErrorR("Test error log")

	// Assert
	if err == nil || err.Error() != "Test error log" {
		t.Fatalf("expected error 'Test error log', got %v", err)
	}
	if len(hook.Entries) != 1 {
		t.Fatalf("expected 1 log entry, got %d", len(hook.Entries))
	}

	entry := hook.Entries[0]
	if entry.Message != "Test error log" {
		t.Errorf("unexpected message: %v", entry.Message)
	}
	if entry.Level != logrus.ErrorLevel {
		t.Errorf("unexpected log level: %v", entry.Level)
	}
	if entry.Data["component"] != "test-component" {
		t.Errorf("unexpected component field: %v", entry.Data["component"])
	}
}

func TestLoggerDebug(t *testing.T) {
	// Arrange
	logger := NewLogger("test-component")
	hook := test.NewLocal(logger.logger)
	defer hook.Reset()

	// Act
	logger.Debug("Test debug log")

	// Assert
	if len(hook.Entries) != 1 {
		t.Fatalf("expected 1 log entry, got %d", len(hook.Entries))
	}

	entry := hook.Entries[0]
	if entry.Message != "Test debug log" {
		t.Errorf("unexpected message: %v", entry.Message)
	}
	if entry.Level != logrus.DebugLevel {
		t.Errorf("unexpected log level: %v", entry.Level)
	}
	if entry.Data["component"] != "test-component" {
		t.Errorf("unexpected component field: %v", entry.Data["component"])
	}
}

func TestLoggerWarn(t *testing.T) {
	// Arrange
	logger := NewLogger("test-component")
	hook := test.NewLocal(logger.logger)
	defer hook.Reset()

	// Act
	logger.Warn("Test warn log")

	// Assert
	if len(hook.Entries) != 1 {
		t.Fatalf("expected 1 log entry, got %d", len(hook.Entries))
	}

	entry := hook.Entries[0]
	if entry.Message != "Test warn log" {
		t.Errorf("unexpected message: %v", entry.Message)
	}
	if entry.Level != logrus.WarnLevel {
		t.Errorf("unexpected log level: %v", entry.Level)
	}
	if entry.Data["component"] != "test-component" {
		t.Errorf("unexpected component field: %v", entry.Data["component"])
	}
}

func TestLoggerFatal(t *testing.T) {
	// Arrange
	logger := NewLogger("test-component")
	hook := test.NewLocal(logger.logger)
	defer hook.Reset()

	// Override the exit function to prevent os.Exit from being called
	exitFunc := logger.ExitFunc
	defer func() { logger.ExitFunc = exitFunc }()
	logger.ExitFunc = func(code int) {
		// Prevent os.Exit from being called
	}

	// Act
	logger.Fatal("Test fatal log")

	// Assert
	if len(hook.Entries) != 1 {
		t.Fatalf("expected 1 log entry, got %d", len(hook.Entries))
	}

	entry := hook.Entries[0]
	if entry.Message != "Test fatal log" {
		t.Errorf("unexpected message: %v", entry.Message)
	}
	if entry.Level != logrus.FatalLevel {
		t.Errorf("unexpected log level: %v", entry.Level)
	}
	if entry.Data["component"] != "test-component" {
		t.Errorf("unexpected component field: %v", entry.Data["component"])
	}
}
