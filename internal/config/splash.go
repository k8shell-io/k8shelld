package config

import (
	"bytes"
	"os"
	"strings"
	"text/template"
	"time"
)

// SplashVars holds all values that can be referenced inside the splash template.
// Use Go template syntax in the splash YAML field, e.g.:
//
//	splash: "k8shelld {{.Version}} | user: {{.Username}} | host: {{.Hostname}}"
//
// To add a new variable in the future, simply add a field here and populate it
// in ExpandSplash — no changes are needed anywhere else.
type SplashVars struct {
	// Version is the running k8shelld version (e.g. "1.2.3").
	Version string
	// Commit is the short git commit hash baked in at build time.
	Commit string
	// Username is the primary OS user configured for this instance.
	Username string
	// ShellUsername is the OS user that the current shell session runs as.
	// This may differ from Username when the caller requested a different user.
	ShellUsername string
	// Hostname is the hostname of the container / machine.
	Hostname string
	// Date is today's date formatted as YYYY-MM-DD.
	Date string
}

// ExpandSplash executes the splash string as a Go template with the supplied
// variables and normalises newlines to \r\n for correct PTY rendering.
// If template parsing or execution fails the original splash string is returned
// unchanged so a misconfigured template never breaks the shell startup.
func ExpandSplash(splash, shellUsername string) string {
	if splash == "" {
		return ""
	}

	hostname, _ := os.Hostname()

	vars := SplashVars{
		Version:       K8SHELLD_VERSION,
		Commit:        K8SHELLD_COMMIT,
		Username:      "", // populated by the caller via withUsername if needed
		ShellUsername: shellUsername,
		Hostname:      hostname,
		Date:          time.Now().Format("2006-01-02"),
	}

	tmpl, err := template.New("splash").Parse(splash)
	if err != nil {
		return splash
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, vars); err != nil {
		return splash
	}

	result := strings.ReplaceAll(buf.String(), "\r\n", "\n") // normalise first
	result = strings.ReplaceAll(result, "\n", "\r\n")
	if !strings.HasSuffix(result, "\r\n") {
		result += "\r\n"
	}
	return result
}

// ExpandSplashWithConfig is a convenience wrapper that also fills Username from
// the Config and accepts the active shell username separately.
func (c *Config) ExpandSplash(shellUsername string) string {
	if c.Splash == "" {
		return ""
	}

	hostname, _ := os.Hostname()

	vars := SplashVars{
		Version:       K8SHELLD_VERSION,
		Commit:        K8SHELLD_COMMIT,
		Username:      c.User.Username,
		ShellUsername: shellUsername,
		Hostname:      hostname,
		Date:          time.Now().Format("2006-01-02"),
	}

	tmpl, err := template.New("splash").Parse(c.Splash)
	if err != nil {
		return c.Splash
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, vars); err != nil {
		return c.Splash
	}

	result := strings.ReplaceAll(buf.String(), "\r\n", "\n")
	result = strings.ReplaceAll(result, "\n", "\r\n")
	if !strings.HasSuffix(result, "\r\n") {
		result += "\r\n"
	}
	return result
}
