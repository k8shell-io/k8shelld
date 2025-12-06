package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/k8shell-io/k8shelld/internal/client"
	"github.com/k8shell-io/k8shelld/internal/table"
	"github.com/spf13/cobra"
)

func appsRowColorFunc(row map[string]interface{}) string {
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
		Width: 10,
	},
	{
		Name:  "version",
		Width: 12,
	},
	{
		Name:  "port",
		Width: 5,
		Type:  table.Int,
	},
	{
		Name:  "pid",
		Width: 5,
		Type:  table.Int,
	},
	{
		Name:  "age",
		Width: 7,
	},
	{
		Name:  "restarts",
		Width: 10,
		Type:  table.Int,
	},
}

var AppsCmd = &cobra.Command{
	Use:   "apps",
	Short: "Manage k8shelld apps",
	Long:  `Manage k8shelld apps.`,
}

var AppsLsCmd = &cobra.Command{
	Use:   "ls",
	Short: "List apps and their status",
	Run: func(cmd *cobra.Command, args []string) {
		resp, err := client.MakeRequest("GET", "/apps", nil, nil)
		if err != nil {
			fmt.Printf("Failed to get apps status: %v\n", err)
			return
		}
		defer resp.Body.Close()

		err = client.CheckApplicationError(resp)
		if err != nil {
			fmt.Printf("%v\n", err)
			return
		}

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

		err = client.CheckApplicationError(resp)
		if err != nil {
			fmt.Printf("%v\n", err)
			return
		}
		fmt.Printf("App %s installation started.\n", name)
	},
}

var AppsLogsCmd = &cobra.Command{
	Use:   "logs <app-name>",
	Short: "Show latest logs",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		name := args[0]
		install, _ := cmd.Flags().GetBool("install")
		follow, _ := cmd.Flags().GetBool("follow")
		logType := "app"
		if install {
			logType = "install"
		}

		url := fmt.Sprintf("/apps/%s/logs?logType=%s", name, logType)
		if follow {
			url += "&follow=true"
		}

		resp, err := client.MakeRequest("GET", url, nil, nil)
		if err != nil {
			fmt.Printf("Failed to get %s logs for app %s: %v\n", logType, name, err)
			return
		}
		defer resp.Body.Close()

		err = client.CheckApplicationError(resp)
		if err != nil {
			fmt.Printf("%v\n", err)
			return
		}

		if follow {
			if _, err := io.Copy(os.Stdout, resp.Body); err != nil {
				fmt.Printf("Error while streaming %s log: %v\n", logType, err)
			}
			return
		}

		body, _ := io.ReadAll(resp.Body)
		if len(body) == 0 {
			fmt.Printf("No %s log available for app %s.\n", logType, name)
			return
		}

		fmt.Print(string(body))
	},
}

var AppsStartCmd = &cobra.Command{
	Use:   "start <app-name>",
	Short: "Start and supervise an app",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		name := args[0]

		url := fmt.Sprintf("/apps/%s/start", name)

		resp, err := client.MakeRequest("POST", url, nil, nil)
		if err != nil {
			fmt.Printf("Failed to start app %q: %v\n", name, err)
			return
		}
		defer resp.Body.Close()

		err = client.CheckApplicationError(resp)
		if err != nil {
			fmt.Printf("%v\n", err)
			return
		}

		fmt.Printf("App %s started.\n", name)
	},
}

// NEW: stop app (stop supervising and stop process)
var AppsStopCmd = &cobra.Command{
	Use:   "stop <app-name>",
	Short: "Stop an app and its supervision",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		name := args[0]

		url := fmt.Sprintf("/apps/%s/stop", name)

		resp, err := client.MakeRequest("POST", url, nil, nil)
		if err != nil {
			fmt.Printf("Failed to stop app %q: %v\n", name, err)
			return
		}
		defer resp.Body.Close()

		err = client.CheckApplicationError(resp)
		if err != nil {
			fmt.Printf("%v\n", err)
			return
		}

		fmt.Printf("App %s stopped.\n", name)

	},
}

func init() {
	AppsLsCmd.Flags().String("sort", "name",
		"Comma separated list of fields to sort by, prefix with '-' for descending order")
	AppsLsCmd.Flags().Bool("json", false, "Display output in JSON format")
	AppsLsCmd.Flags().Bool("no-ansi", false, "Disable ANSI color output")

	AppsInstallCmd.Flags().BoolP("force", "", false, "Reinstall the app if already installed")
	AppsLogsCmd.Flags().BoolP("follow", "f", false, "Follow log output")
	AppsLogsCmd.Flags().BoolP("install", "i", false, "Show install log")

	AppsCmd.AddCommand(AppsLsCmd)
	AppsCmd.AddCommand(AppsInstallCmd)
	AppsCmd.AddCommand(AppsLogsCmd)
	AppsCmd.AddCommand(AppsStartCmd)
	AppsCmd.AddCommand(AppsStopCmd)
}
