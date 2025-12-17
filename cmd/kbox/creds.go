package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/k8shell-io/k8shelld/internal/client"
	"github.com/spf13/cobra"
)

var operation string

var CredsCmd = &cobra.Command{
	Use:   "creds",
	Short: "Credentials helpers",
	Long:  "Credentials helpers for Docker and Git",
	Run: func(cmd *cobra.Command, args []string) {
		cmd.Help()
	},
}

var dockerCredsHelperCmd = &cobra.Command{
	Use:   "docker",
	Short: "Docker credentials helper",
	Long:  "Docker credentials helper",
	Run: func(cmd *cobra.Command, args []string) {
		dockerCredsHelper(operation)
	},
}

var gitCredsHelperCmd = &cobra.Command{
	Use:   "git",
	Short: "Git credentials helper",
	Long:  "Git credentials helper",
	Run: func(cmd *cobra.Command, args []string) {
		gitCredsHelper(operation)
	},
}

func init() {
	CredsCmd.AddCommand(dockerCredsHelperCmd)
	CredsCmd.AddCommand(gitCredsHelperCmd)

	dockerCredsHelperCmd.Flags().StringVarP(&operation, "oper", "o", "get", "Operation to perform")
	gitCredsHelperCmd.Flags().StringVarP(&operation, "oper", "o", "get", "Operation to perform")
}

// dockerCredsHelper implements Docker credentials helper protocol
// It reads from stdin and writes to stdout as per Docker's credentials helper protocol.
func dockerCredsHelper(operation string) {
	switch operation {
	case "get":
		scanner := bufio.NewScanner(os.Stdin)
		if !scanner.Scan() {
			fmt.Println("No address provided. Please provide a Docker registry address.")
			os.Exit(1)
		}

		address := strings.TrimSpace(scanner.Text())

		url := fmt.Sprintf("/creds?type=docker&address=%s", address)
		headers := map[string]string{"Accept": "application/json"}

		resp, err := client.MakeRequest("GET", url, headers, nil)
		if err != nil {
			fmt.Println("{}")
			fmt.Printf("Failed to get credentials: %v\n", err)
			os.Exit(0)
		}
		defer resp.Body.Close()

		err = client.CheckApplicationError(resp)
		if err != nil {
			fmt.Printf("%v\n", err)
			return
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

// gitCredsHelper implements Git credential helper protocol
func gitCredsHelper(operation string) {
	creds := map[string]string{}
	scanner := bufio.NewScanner(os.Stdin)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			break
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			creds[parts[0]] = parts[1]
		}
	}

	switch operation {
	case "get":
		url := fmt.Sprintf("/creds?type=git&address=%s", creds["host"])
		resp, err := client.MakeRequest("GET", url, map[string]string{"Accept": "application/json"}, nil)
		if err != nil {
			fmt.Fprint(os.Stdout, "\n")
			return
		}
		defer resp.Body.Close()

		err = client.CheckApplicationError(resp)
		if err != nil {
			fmt.Printf("%v\n", err)
			return
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			fmt.Fprint(os.Stdout, "\n")
			return
		}
		username, password := extractCredsFromJSON(body)
		fmt.Fprintf(os.Stdout, "username=%s\n", username)
		fmt.Fprintf(os.Stdout, "password=%s\n\n", password)

	case "store":
		fmt.Println("Storing credentials is not supported.")
	case "erase":
		fmt.Println("Erasing credentials is not supported.")
	default:
		fmt.Fprintf(os.Stderr, "Unknown operation: %s\n", operation)
		os.Exit(1)
	}
}

// extractCredsFromJSON extracts username and password from the given JSON byte slice.
func extractCredsFromJSON(data []byte) (string, string) {
	var creds struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.Unmarshal(data, &creds); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to parse credentials JSON: %v\n", err)
		return "", ""
	}
	return creds.Username, creds.Password
}
