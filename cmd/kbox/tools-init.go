package main

import (
	"fmt"
	"html/template"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

var (
	outputDir string
)

const (
	wrapperScriptGeneralTemplateHelp = `#!/bin/sh
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

	wrapperScriptGeneralTemplateNoHelp = `#!/bin/sh
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

// toolsInitCmd represents the tools-init command
var ToolsInitCmd = &cobra.Command{
	Use:   "tools-init",
	Short: "Generate wrapper scripts for system tools",
	Long: `Generate wrapper scripts for system tools.
These scripts wrap the corresponding sub-commands in the kbox CLI.`,
	Run: func(cmd *cobra.Command, args []string) {
		toolName := "kbox"

		// Array of wrapper configuration
		wrappers := []struct {
			CommandName string
			ScriptName  string
			Template    string
		}{
			{
				CommandName: "uptime",
				ScriptName:  "uptime",
				Template:    wrapperScriptGeneralTemplateHelp,
			},
			{
				CommandName: "last",
				ScriptName:  "last",
				Template:    wrapperScriptGeneralTemplateHelp,
			},
			{
				CommandName: "shutdown",
				ScriptName:  "shutdown",
				Template:    wrapperScriptGeneralTemplateHelp,
			},
			{
				CommandName: "creds docker",
				ScriptName:  "docker-credential-k8shell",
				Template:    wrapperScriptGeneralTemplateNoHelp,
			},
			{
				CommandName: "creds git",
				ScriptName:  "git-credential-k8shell",
				Template:    wrapperScriptGeneralTemplateNoHelp,
			},
		}

		if err := os.MkdirAll(outputDir, 0o750); err != nil {
			fmt.Printf("Failed to create output directory %s: %v\n", outputDir, err)
			os.Exit(1)
		}

		for _, wrapper := range wrappers {
			if err := generateWrapper(outputDir, toolName, wrapper.CommandName, wrapper.ScriptName, wrapper.Template); err != nil {
				fmt.Printf("Error generating wrapper for %s: %v\n", wrapper.CommandName, err)
			}
		}

		fmt.Printf("Wrapper scripts generated in %s\n", outputDir)
	},
}

func generateWrapper(outputDir, toolName, commandName, scriptName, temp string) error {
	scriptPath := filepath.Join(outputDir, scriptName)
	file, err := os.Create(scriptPath)
	if err != nil {
		return fmt.Errorf("failed to create script for %s: %v", commandName, err)
	}
	defer file.Close()

	tmpl, err := template.New("wrapper").Parse(temp)
	if err != nil {
		return fmt.Errorf("failed to parse template: %v", err)
	}

	data := wrapperScriptData{
		ToolName:    toolName,
		CommandName: commandName,
	}

	if err := tmpl.Execute(file, data); err != nil {
		return fmt.Errorf("failed to execute template for %s: %v", commandName, err)
	}

	if err := os.Chmod(scriptPath, 0o700); err != nil {
		return fmt.Errorf("failed to make script executable for %s: %v", commandName, err)
	}

	return nil
}

func init() {
	ToolsInitCmd.Flags().StringVarP(&outputDir, "output", "o", "/usr/local/bin", "Directory to generate wrapper scripts")
}
