package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/k8shell-io/k8shelld/internal/client"
	"github.com/spf13/cobra"
)

var operation string

var CredentialsCmd = &cobra.Command{
	Use:   "credentials",
	Short: "Credentials helpers",
	Long:  "Credentials helpers for Docker and Git",
	Run: func(cmd *cobra.Command, args []string) {
		_ = cmd.Help()
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

var kubernetesCredsHelperCmd = &cobra.Command{
	Use:   "kubernetes",
	Short: "Kubernetes credentials helper",
	Long:  "Kubernetes exec credentials helper (client.authentication.k8s.io/v1beta1)",
	Run: func(cmd *cobra.Command, args []string) {
		kubernetesCredsHelper(operation)
	},
}

func init() {
	CredentialsCmd.AddCommand(dockerCredsHelperCmd)
	CredentialsCmd.AddCommand(gitCredsHelperCmd)
	CredentialsCmd.AddCommand(kubernetesCredsHelperCmd)

	dockerCredsHelperCmd.Flags().StringVarP(&operation, "oper", "o", "get", "Operation to perform")
	gitCredsHelperCmd.Flags().StringVarP(&operation, "oper", "o", "get", "Operation to perform")
	kubernetesCredsHelperCmd.Flags().StringVarP(&operation, "oper", "o", "get", "Operation to perform")
}

type dockerGetResponse struct {
	Username string `json:"Username"`
	Secret   string `json:"Secret"`
}

// validateDockerCredsJSON enforces what Docker expects from a credential helper "get":
// a JSON object with non-empty Username and Secret.
// It returns canonical JSON (marshaled from struct) to avoid forwarding API noise/extra fields.
func validateDockerCredsJSON(body []byte) ([]byte, bool) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return nil, false
	}

	// First unmarshal strictly into the expected shape.
	var resp dockerGetResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, false
	}

	// Require both fields and non-empty values.
	if strings.TrimSpace(resp.Username) == "" || strings.TrimSpace(resp.Secret) == "" {
		return nil, false
	}

	// Canonicalize output: only emit the exact fields Docker uses.
	canonical, err := json.Marshal(resp)
	if err != nil {
		return nil, false
	}

	return canonical, true
}

// dockerCredsHelper implements Docker credentials helper protocol
// It reads from stdin and writes to stdout as per Docker's credentials helper protocol.
func dockerCredsHelper(operation string) {
	switch operation {
	case "get":
		scanner := bufio.NewScanner(os.Stdin)
		if !scanner.Scan() {
			fmt.Fprintln(os.Stderr, "No address provided.")
			os.Exit(1)
		}
		address := strings.TrimSpace(scanner.Text())

		urlPath := fmt.Sprintf("/creds?type=docker&address=%s", url.QueryEscape(address))
		headers := map[string]string{"Accept": "application/json"}

		resp, err := client.MakeRequest("GET", urlPath, headers, nil)
		if err != nil {
			os.Exit(1)
		}
		defer resp.Body.Close()

		bodyBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			os.Exit(1)
		}

		switch resp.StatusCode {
		case 200:
			if canonical, ok := validateDockerCredsJSON(bodyBytes); ok {
				fmt.Print(string(canonical))
				os.Exit(0)
			}
			fmt.Print("{}")
			os.Exit(0)

		case 204, 404, 401, 403:
			fmt.Print("{}")
			os.Exit(0)

		default:
			os.Exit(1)
		}

	case "list":
		fmt.Print("{}")
		os.Exit(0)

	case "store", "erase":
		os.Exit(0)

	default:
		fmt.Fprintf(os.Stderr, "Invalid operation: %s\n", operation)
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
		url := fmt.Sprintf("/creds?type=git&protocol=%s&address=%s",
			url.QueryEscape(creds["protocol"]), url.QueryEscape(creds["host"]))
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

// kubernetesCredsHelper implements the kubectl exec credential plugin protocol
// (client.authentication.k8s.io/v1beta1). It fetches a token from the k8shelld
// REST API and returns it as an ExecCredential JSON object.
func kubernetesCredsHelper(operation string) {
	if operation != "get" {
		os.Exit(0)
	}

	resp, err := client.MakeRequest("GET", "/creds?type=kubernetes", map[string]string{"Accept": "application/json"}, nil)
	if err != nil {
		os.Exit(1)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		os.Exit(1)
	}

	switch resp.StatusCode {
	case 200:
		var cred struct {
			Secret    string  `json:"secret"`
			ExpiresAt *string `json:"expiresAt"` // RFC3339, optional
		}
		if err := json.Unmarshal(bodyBytes, &cred); err != nil || cred.Secret == "" {
			os.Exit(1)
		}
		status := fmt.Sprintf(`"token":%q`, cred.Secret)
		if cred.ExpiresAt != nil && *cred.ExpiresAt != "" {
			status += fmt.Sprintf(`,"expirationTimestamp":%q`, *cred.ExpiresAt)
		}
		fmt.Printf(`{"apiVersion":"client.authentication.k8s.io/v1beta1","kind":"ExecCredential","status":{%s}}`, status)
		os.Exit(0)
	case 204, 404, 401, 403:
		os.Exit(0)
	default:
		os.Exit(1)
	}
}
