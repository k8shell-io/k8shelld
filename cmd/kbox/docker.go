package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/k8shell-io/k8shelld/internal/client"

	"github.com/spf13/cobra"
)

var (
	operation string
)

var DockerCmd = &cobra.Command{
	Use:   "docker",
	Short: "Credentials helper and DNS commands",
	Long:  "Credentials helper and DNS commands",
	Run: func(cmd *cobra.Command, args []string) {
		cmd.Help()
	},
}

var credsHelperCmd = &cobra.Command{
	Use:   "creds",
	Short: "Docker credentials helper for k8shell",
	Long:  "Docker credentials helper for k8shell",
	Run: func(cmd *cobra.Command, args []string) {
		DockerCredsHelper(operation)
	},
}

var dnsCmd = &cobra.Command{
	Use:   "dns",
	Short: "Docker container names resolution commands",
	Long:  "Docker container names resolution commands",
}

var dnsEnableCmd = &cobra.Command{
	Use:   "enable",
	Short: "Enable container names resolutions",
	Long:  "Enable container names resolutions",
	Run: func(cmd *cobra.Command, args []string) {
		UpdateDNSStatus("enabled")
	},
}

var dnsDisableCmd = &cobra.Command{
	Use:   "disable",
	Short: "Disable container names resolutions",
	Long:  "Disable container names resolutions",
	Run: func(cmd *cobra.Command, args []string) {
		UpdateDNSStatus("disabled")
	},
}

var dnsStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Get container names resolution status",
	Long:  "Get container names resolution status",
	Run: func(cmd *cobra.Command, args []string) {
		url := "/docker/dns"
		resp, err := client.MakeRequest("GET", url, nil, nil)
		if err != nil {
			fmt.Printf("Failed to get DNS status: %v\n", err)
			return
		}
		defer resp.Body.Close()

		statusMap := map[string]string{}
		bodyBytes, _ := io.ReadAll(resp.Body)
		json.Unmarshal(bodyBytes, &statusMap)

		if resp.StatusCode != http.StatusOK {
			fmt.Printf("Failed to get DNS status, status: %s\n", resp.Status)
		} else {
			fmt.Printf("DNS status is '%s'\n", statusMap["status"])
		}

	},
}

func init() {
	DockerCmd.AddCommand(credsHelperCmd)
	DockerCmd.AddCommand(dnsCmd)

	dnsCmd.AddCommand(dnsEnableCmd)
	dnsCmd.AddCommand(dnsDisableCmd)
	dnsCmd.AddCommand(dnsStatusCmd)

	credsHelperCmd.Flags().StringVarP(&operation, "oper", "o", "get", "Operation to perform")
}

func DockerCredsHelper(operation string) {
	switch operation {
	case "get":
		scanner := bufio.NewScanner(os.Stdin)
		if !scanner.Scan() {
			fmt.Println("No address provided. Please provide a Docker registry address.")
			os.Exit(1)
		}

		address := strings.TrimSpace(scanner.Text())

		url := fmt.Sprintf("/docker/creds-helper?address=%s", address)
		headers := map[string]string{"Accept": "application/json"}

		resp, err := client.MakeRequest("GET", url, headers, nil)
		if err != nil {
			fmt.Println("{}")
			fmt.Printf("Failed to get credentials: %v\n", err)
			os.Exit(0)
		}

		bodyBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			fmt.Println("{}")
			fmt.Printf("Failed to read response body: %v\n", err)
			os.Exit(0)
		}
		fmt.Println(string(bodyBytes))

	case "store":
		fmt.Println("Request to store credentials, operation not supported.")

	case "erase":
		fmt.Println("Request to erase credentials, operation not supported.")

	default:
		fmt.Printf("Invalid operation: %s\n", operation)
		os.Exit(1)
	}
}

func UpdateDNSStatus(status string) {
	url := "/docker/dns"
	statusMap := map[string]string{
		"status": status,
	}
	jsonData, _ := json.Marshal(statusMap)
	resp, err := client.MakeRequest("PATCH", url, nil, bytes.NewReader(jsonData))
	if err != nil {
		fmt.Printf("Failed to update DNS status: %v\n", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Printf("Failed to update DNS status, status: %s\n", resp.Status)
	} else {
		fmt.Printf("DNS status set to '%s'\n", status)
	}
}
