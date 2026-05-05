package server

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"text/template"
)

const (
	wrapperOutputDir = "/usr/local/bin"
	kboxBin          = "kbox"

	wrapperTemplateHelp = `#!/bin/sh
COMMAND_NAME="{{ .CommandName }}"
SCRIPT_DIR=$(dirname "$0")
KBOX_PATH="$SCRIPT_DIR/{{ .ToolName }}"
if [ "$1" = "-h" ] || [ "$1" = "--help" ]; then
    HELP_MSG=$("$KBOX_PATH" "$COMMAND_NAME" --help)
    echo "$HELP_MSG" | sed "s/{{ .ToolName }} $COMMAND_NAME/$COMMAND_NAME/g"
    exit 0
fi
$KBOX_PATH $COMMAND_NAME "$@"
`

	wrapperTemplateNoHelp = `#!/bin/sh
COMMAND_NAME="{{ .CommandName }}"
SCRIPT_DIR=$(dirname "$0")
KBOX_PATH="$SCRIPT_DIR/{{ .ToolName }}"
if [ "$1" = "-h" ] || [ "$1" = "--help" ]; then
    exit 0
fi
$KBOX_PATH $COMMAND_NAME "$@"
`
)

type wrapperScriptData struct {
	ToolName    string
	CommandName string
}

// setupToolWrappers generates kbox wrapper scripts in wrapperOutputDir.
// Wrappers for credential helpers are only created when the corresponding
// service is available.
func (s *Server) setupToolWrappers() {
	type wrapperSpec struct {
		CommandName string
		ScriptName  string
		Template    string
	}

	wrappers := []wrapperSpec{
		{"uptime", "uptime", wrapperTemplateHelp},
		{"last", "last", wrapperTemplateHelp},
		{"shutdown", "shutdown", wrapperTemplateHelp},
	}

	if s.blueprint != nil && s.blueprint.Podman.Enabled {
		wrappers = append(wrappers, wrapperSpec{
			CommandName: "credentials docker",
			ScriptName:  "docker-credential-k8shell",
			Template:    wrapperTemplateNoHelp,
		})
	}

	if s.blueprint != nil && s.blueprint.Metadata.RepoAddress != "" {
		if _, err := exec.LookPath("git"); err == nil {
			wrappers = append(wrappers, wrapperSpec{
				CommandName: "credentials git",
				ScriptName:  "git-credential-k8shell",
				Template:    wrapperTemplateNoHelp,
			})
		}
	}

	if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		wrappers = append(wrappers, wrapperSpec{
			CommandName: "credentials kubernetes",
			ScriptName:  "kubectl-credential-k8shell",
			Template:    wrapperTemplateNoHelp,
		})
	}

	if err := os.MkdirAll(wrapperOutputDir, 0o755); err != nil {
		s.logger.Error().Msgf("Failed to create wrapper output dir %s: %v", wrapperOutputDir, err)
		return
	}

	for _, w := range wrappers {
		if err := writeWrapperScript(wrapperOutputDir, kboxBin, w.CommandName, w.ScriptName, w.Template); err != nil {
			s.logger.Error().Msgf("Failed to create wrapper %s: %v", w.ScriptName, err)
		}
	}
}

func writeWrapperScript(outputDir, toolName, commandName, scriptName, tmplText string) error {
	tmpl, err := template.New("wrapper").Parse(tmplText)
	if err != nil {
		return fmt.Errorf("parse template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, wrapperScriptData{ToolName: toolName, CommandName: commandName}); err != nil {
		return fmt.Errorf("execute template: %w", err)
	}

	scriptPath := filepath.Join(outputDir, scriptName)
	if err := os.WriteFile(scriptPath, buf.Bytes(), 0o755); err != nil {
		return fmt.Errorf("write %s: %w", scriptPath, err)
	}
	return nil
}
