package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/k8shell-io/k8shelld/internal/client"
	"github.com/k8shell-io/k8shelld/internal/table"
	"github.com/spf13/cobra"
)

func appsRowColorFunc(row map[string]interface{}) string {
	// Dim non‑running / invalid apps
	if status, ok := row["status"].(string); ok && status != "RUNNING" {
		return "\033[2m"
	}
	return ""
}

var appsTableFields = []table.FieldDefinition{
	{
		Name:  "name",
		Width: 14,
	},
	{
		Name:  "status",
		Width: 9,
	},
	{
		Name:  "version",
		Width: 12,
	},
	{
		Name:  "listen_port",
		Width: 11,
		Type:  table.Int,
	},
	{
		Name:  "pid",
		Width: 7,
		Type:  table.Int,
	},
	{
		Name:  "running_time",
		Width: 14,
	},
}

var AppsCmd = &cobra.Command{
	Use:   "apps",
	Short: "Display apps managed by k8shelld",
	Long: `Display apps managed by k8shelld and their status.

Fields:
- name: App name
- status: INSTALLED, RUNNING, or INVALID
- version: Detected version (if any)
- listen_port: TCP port the app is expected to listen on (0 if none)
- pid: PID of detected running process (0 if not found)
- running_time: How long the process has been running (e.g. "3m12s")`,
	Run: func(cmd *cobra.Command, args []string) {
		url := "/apps"
		resp, err := client.MakeRequest("GET", url, nil, nil)
		if err != nil {
			fmt.Printf("Failed to get apps status: %v\n", err)
			return
		}
		defer resp.Body.Close()

		bodyBytes, _ := io.ReadAll(resp.Body)

		var t *table.Table
		if noAnsi, _ := cmd.Flags().GetBool("no-ansi"); noAnsi {
			t = table.NewTable(appsTableFields, nil)
		} else {
			t = table.NewTable(appsTableFields, appsRowColorFunc)
		}

		if err := t.ParseJsonData(bodyBytes); err != nil {
			fmt.Printf("Error parsing JSON data: %v\n", err)
			return
		}

		sort := cmd.Flag("sort").Value.String()
		if sort != "" {
			if err := t.Sort(strings.Split(sort, ",")); err != nil {
				fmt.Printf("Error sorting table: %v\n", err)
				return
			}
		}

		if jsonFlag, _ := cmd.Flags().GetBool("json"); jsonFlag {
			if err := t.DisplayJSON(); err != nil {
				fmt.Printf("Error displaying JSON: %v\n", err)
			}
			return
		}

		if err := t.DisplayTable(); err != nil {
			fmt.Printf("Error displaying table: %v\n", err)
		}
	},
}

var AppsInstallCmd = &cobra.Command{
	Use:   "install <app-name>",
	Short: "Install an app",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		name := args[0]

		force, _ := cmd.Flags().GetBool("force")

		url := fmt.Sprintf("/apps/%s/install", name)
		if force {
			url += "?force=true"
		}

		resp, err := client.MakeRequest("POST", url, nil, nil)
		if err != nil {
			fmt.Printf("Failed to install app %q: %v\n", name, err)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != 202 && resp.StatusCode != 200 {
			body, _ := io.ReadAll(resp.Body)
			fmt.Printf("Install failed for %q: %s (%s)\n", name, resp.Status, string(body))
			return
		}

		fmt.Printf("App %q installation started.\n", name)
	},
}

var AppsLogCmd = &cobra.Command{
	Use:   "log <app-name>",
	Short: "Show latest install log for an app",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		name := args[0]

		url := fmt.Sprintf("/apps/%s/install/log", name)
		resp, err := client.MakeRequest("GET", url, nil, nil)
		if err != nil {
			fmt.Printf("Failed to get install log for app %q: %v\n", name, err)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != 200 {
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode == 404 {
				fmt.Printf("No install log found for app %q\n", name)
				return
			}
			fmt.Printf("Failed to get install log for %q: %s (%s)\n",
				name, resp.Status, strings.TrimSpace(string(body)))
			return
		}

		body, _ := io.ReadAll(resp.Body)
		if len(body) == 0 {
			fmt.Printf("No install log found for app %q\n", name)
			return
		}

		fmt.Print(string(body))
	},
}

func init() {
	AppsCmd.Flags().String("sort", "name",
		"Comma separated list of fields to sort by, prefix with '-' for descending order")
	AppsCmd.Flags().Bool("json", false, "Display output in JSON format")
	AppsCmd.Flags().Bool("no-ansi", false, "Disable ANSI color output")
	AppsInstallCmd.Flags().BoolP("force", "f", false, "Reinstall the app if already installed")

	AppsCmd.AddCommand(AppsInstallCmd)
	AppsCmd.AddCommand(AppsLogCmd)
}
