package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/sirupsen/logrus"
)

var UseJSONFormatter = true

type LogMessageRequest struct {
	Time      string `json:"time"`
	Level     string `json:"level"`
	Component string `json:"component"`
	Message   string `json:"message"`
}

type OrderedJSONFormatter struct{}

var LogLevel = logrus.DebugLevel

func SetLogLevel(level string) error {
	l, err := logrus.ParseLevel(level)
	if err != nil {
		return err
	}
	LogLevel = l
	return nil
}

func (f *OrderedJSONFormatter) Format(entry *logrus.Entry) ([]byte, error) {
	data := struct {
		Time      string `json:"time"`
		Component string `json:"component,omitempty"`
		Level     string `json:"level"`
		Message   string `json:"message"`
	}{
		Time:      entry.Time.Format("2006-01-02T15:04:05.000Z07:00"),
		Component: fmt.Sprintf("%v", entry.Data["component"]),
		Level:     entry.Level.String(),
		Message:   entry.Message,
	}
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	err := encoder.Encode(data)
	return buf.Bytes(), err
}

type Logger struct {
	logger    *logrus.Logger
	component string
	ExitFunc  func(int)
}

func NewLogger(component string) *Logger {
	log := logrus.New()
	log.SetOutput(os.Stdout)
	if UseJSONFormatter {
		log.SetFormatter(&OrderedJSONFormatter{})
	} else {
		log.SetFormatter(&logrus.TextFormatter{
			DisableColors: false,
			DisableQuote:  true,
		})
	}
	log.SetLevel(LogLevel)
	return &Logger{
		logger:    log,
		component: component,
		ExitFunc:  func(code int) { os.Exit(code) },
	}
}

func (l *Logger) NewEntry() *logrus.Entry {
	return l.logger.WithField("component", l.component)
}

func (l *Logger) Info(format string, args ...interface{}) {
	l.NewEntry().Info(fmt.Sprintf(format, args...))
}

func (l *Logger) Error(format string, args ...interface{}) {
	l.NewEntry().Error(fmt.Sprintf(format, args...))
}

func (l *Logger) ErrorR(format string, args ...interface{}) error {
	error := fmt.Errorf(format, args...)
	l.NewEntry().Error(fmt.Sprintf(format, args...))
	return error
}

func (l *Logger) Debug(format string, args ...interface{}) {
	l.NewEntry().Debug(fmt.Sprintf(format, args...))
}

func (l *Logger) Warn(format string, args ...interface{}) {
	l.NewEntry().Warn(fmt.Sprintf(format, args...))
}

func (l *Logger) Fatal(format string, args ...interface{}) {
	l.NewEntry().Fatal(fmt.Sprintf(format, args...))
	time.Sleep(100 * time.Millisecond)
	l.ExitFunc(1)
}
