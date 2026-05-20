package display

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/k8shell-io/k8shelld/internal/models"
)

// spinnerFrames is the set of characters cycled for running scripts.
var spinnerFrames = []string{"|", "/", "-", "\\"}

// SpinnerFrame returns the spinner character for the given tick index.
func SpinnerFrame(tick int) string {
	return spinnerFrames[tick%len(spinnerFrames)]
}

// RenderInitProgress formats a list of init script states into display lines.
// spinner is the current spinner character used for running scripts (pass "" for a final/static render).
// useAnsi controls whether ANSI colour codes are included.
// showHeader prepends a "Running init scripts..." header line when true.
// Each returned string is one line without a trailing newline.
func RenderInitProgress(states []models.InitScriptState, spinner string, useAnsi bool, showHeader bool) []string {
	var lines []string
	if showHeader {
		lines = append(lines, "Running init scripts... (Ctrl+C to skip)")
	}
	for _, s := range states {
		lines = append(lines, formatScriptLine(s, spinner, useAnsi))
	}
	return lines
}

// ClearLines writes ANSI escape sequences that move the cursor up n lines and
// clear each one so the next render overwrites the previous output in-place.
// When n is 0 this is a no-op.
func ClearLines(w io.Writer, n int) {
	for i := 0; i < n; i++ {
		// Move cursor up one line, then erase the entire line.
		_, _ = fmt.Fprint(w, "\033[1A\033[2K")
	}
}

// elapsed returns a human-readable duration string (e.g. "1.2s", "65.0s").
func elapsed(start, end *time.Time) string {
	if start == nil || end == nil {
		return ""
	}
	d := end.Sub(*start)
	return fmt.Sprintf("%.1fs", d.Seconds())
}

// ansiWrap wraps text in an ANSI colour sequence when useAnsi is true.
// code is a standard SGR parameter string (e.g. "32" for green).
func ansiWrap(text, code string, useAnsi bool) string {
	if !useAnsi {
		return text
	}
	return fmt.Sprintf("\033[%sm%s\033[0m", code, text)
}

func formatScriptLine(s models.InitScriptState, spinner string, useAnsi bool) string {
	// Strip the leading "__init_" prefix that the daemon uses internally so
	// what's shown to the user matches the logical script name.
	name := strings.TrimPrefix(s.Name, "__init_")

	switch s.Status {
	case models.InitScriptCompleted:
		dur := elapsed(s.StartedAt, s.FinishedAt)
		if s.HasError {
			mark := ansiWrap("✗", "31", useAnsi) // red cross
			label := ansiWrap(name, "31", useAnsi)
			return fmt.Sprintf("[%s] %s: %s", mark, label, dur)
		}
		mark := ansiWrap("✓", "32", useAnsi) // green tick
		label := ansiWrap(name, "32", useAnsi)
		return fmt.Sprintf("[%s] %s: %s", mark, label, dur)

	case models.InitScriptRunning:
		spin := spinner
		if spin == "" {
			spin = "*"
		}
		mark := ansiWrap(spin, "33", useAnsi) // yellow spinner
		label := ansiWrap(name, "33", useAnsi)
		return fmt.Sprintf("[%s] %s: running...", mark, label)

	default: // pending
		mark := ansiWrap("-", "2", useAnsi) // dim dash
		label := ansiWrap(name, "2", useAnsi)
		status := ansiWrap("waiting...", "2", useAnsi)
		return fmt.Sprintf("[%s] %s: %s", mark, label, status)
	}
}
