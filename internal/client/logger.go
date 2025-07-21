package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
)

// Logger represents a structured logger that sends logs to a Unix socket API server
type Logger struct {
	component string
}

// NewLogger creates a logger that logs to a Unix socket API server
func NewLogger(component string) *Logger {
	return &Logger{
		component: component,
	}
}

// LogMessageRequest represents the payload for logging messages
type LogMessageRequest struct {
	Component string `json:"component"`
	Level     string `json:"level"`
	Message   string `json:"message"`
}

// sendLog sends a log message to the API server
func (l *Logger) sendLog(level, message string) {
	logMessage := LogMessageRequest{
		Component: l.component,
		Level:     level,
		Message:   message,
	}

	data, err := json.Marshal(logMessage)
	if err != nil {
		fmt.Printf("Failed to marshal log message: %v\n", err)
		return
	}

	headers := map[string]string{
		"Content-Type": "application/json",
	}

	resp, err := MakeRequest("POST", "/log", headers, bytes.NewReader(data))
	if err != nil {
		fmt.Printf("Failed to call log: %v\n", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Printf("Failed to log message, status: %s\n", resp.Status)
	}
}

func (l *Logger) Info(format string, args ...interface{}) {
	l.sendLog("info", fmt.Sprintf(format, args...))
}

func (l *Logger) Error(format string, args ...interface{}) {
	l.sendLog("error", fmt.Sprintf(format, args...))
}

func (l *Logger) Debug(format string, args ...interface{}) {
	l.sendLog("debug", fmt.Sprintf(format, args...))
}

func (l *Logger) Warn(format string, args ...interface{}) {
	l.sendLog("warn", fmt.Sprintf(format, args...))
}
