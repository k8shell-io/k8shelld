// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	doccredentials "github.com/docker/docker-credential-helpers/credentials"
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
		action := doccredentials.ActionGet
		if len(args) > 0 {
			action = args[0]
		}
		if err := doccredentials.HandleCommand(&k8shellDockerHelper{}, action, os.Stdin, os.Stdout); err != nil {
			_, _ = fmt.Fprintln(os.Stdout, err)
			os.Exit(1)
		}
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

	gitCredsHelperCmd.Flags().StringVarP(&operation, "oper", "o", "get", "Operation to perform")
	kubernetesCredsHelperCmd.Flags().StringVarP(&operation, "oper", "o", "get", "Operation to perform")
}

type dockerGetResponse struct {
	Username string `json:"Username"`
	Secret   string `json:"Secret"`
}

// k8shellDockerHelper implements credentials.Helper for the docker-credential-k8shell helper.
// Get fetches credentials from the k8shelld REST API. Add and Delete are no-ops
// because k8shell is a read-only credential store.
type k8shellDockerHelper struct{}

func (h *k8shellDockerHelper) Add(creds *doccredentials.Credentials) error { return nil }
func (h *k8shellDockerHelper) Delete(serverURL string) error               { return nil }
func (h *k8shellDockerHelper) List() (map[string]string, error)            { return map[string]string{}, nil }

func (h *k8shellDockerHelper) Get(serverURL string) (string, string, error) {
	urlPath := fmt.Sprintf("/creds?type=docker&address=%s", url.QueryEscape(serverURL))
	headers := map[string]string{"Accept": "application/json"}

	resp, err := client.MakeRequest("GET", urlPath, headers, nil)
	if err != nil {
		return "", "", doccredentials.NewErrCredentialsNotFound()
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", fmt.Errorf("failed to read response: %w", err)
	}

	switch resp.StatusCode {
	case 200:
		var cred dockerGetResponse
		if canonical, ok := validateDockerCredsJSON(bodyBytes); ok {
			if err := json.Unmarshal(canonical, &cred); err == nil {
				return cred.Username, cred.Secret, nil
			}
		}
		return "", "", doccredentials.NewErrCredentialsNotFound()
	case 204, 404:
		return "", "", doccredentials.NewErrCredentialsNotFound()
	case 401, 403:
		return "", "", fmt.Errorf("credential store returned HTTP %d for %s", resp.StatusCode, serverURL)
	default:
		return "", "", fmt.Errorf("credential store returned unexpected HTTP %d for %s", resp.StatusCode, serverURL)
	}
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
	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to scan stdin: %v\n", err)
		return
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

// kubeTokenCacheFile returns the path used to cache the kubernetes token
// between kubectl invocations. Returns "" if the home directory is unavailable.
func kubeTokenCacheFile() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".kube", "cache", "k8shell-credential.json")
}

type kubeTokenCache struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expiresAt,omitempty"` // RFC3339
}

// loadKubeTokenCache returns a cached token if it expires more than 60 seconds
// from now, otherwise nil.
func loadKubeTokenCache() *kubeTokenCache {
	path := kubeTokenCacheFile()
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var ct kubeTokenCache
	if err := json.Unmarshal(data, &ct); err != nil || ct.Token == "" {
		return nil
	}
	if ct.ExpiresAt != "" {
		if t, err := time.Parse(time.RFC3339, ct.ExpiresAt); err == nil {
			if time.Until(t) <= 60*time.Second {
				return nil // expiring soon, force refresh
			}
		}
	}
	return &ct
}

// saveKubeTokenCache writes the token to the cache file, ignoring errors.
func saveKubeTokenCache(ct *kubeTokenCache) {
	path := kubeTokenCacheFile()
	if path == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	data, err := json.Marshal(ct)
	if err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0o600)
}

// kubernetesCredsHelper implements the kubectl exec credential plugin protocol
// (client.authentication.k8s.io/v1beta1). It fetches a token from the k8shelld
// REST API and returns it as an ExecCredential JSON object.
func kubernetesCredsHelper(operation string) {
	if operation != "get" {
		os.Exit(0)
	}

	// Return cached token if still valid (more than 60s until expiry).
	if cached := loadKubeTokenCache(); cached != nil {
		status := fmt.Sprintf(`"token":%q`, cached.Token)
		if cached.ExpiresAt != "" {
			status += fmt.Sprintf(`,"expirationTimestamp":%q`, cached.ExpiresAt)
		}
		fmt.Printf(`{"apiVersion":"client.authentication.k8s.io/v1beta1","kind":"ExecCredential","status":{%s}}`, status)
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
			Secret      string  `json:"secret"`
			ExpiresAt   *string `json:"expiresAt"` // RFC3339, optional
			CacheTokens bool    `json:"cacheTokens"`
		}
		if err := json.Unmarshal(bodyBytes, &cred); err != nil || cred.Secret == "" {
			os.Exit(1)
		}
		expiry := ""
		if cred.ExpiresAt != nil {
			expiry = *cred.ExpiresAt
		}
		if cred.CacheTokens {
			saveKubeTokenCache(&kubeTokenCache{Token: cred.Secret, ExpiresAt: expiry})
		} else {
			// Caching is disabled; remove any previously cached token so it
			// is not served on the next invocation.
			if path := kubeTokenCacheFile(); path != "" {
				_ = os.Remove(path)
			}
		}
		status := fmt.Sprintf(`"token":%q`, cred.Secret)
		if expiry != "" {
			status += fmt.Sprintf(`,"expirationTimestamp":%q`, expiry)
		}
		fmt.Printf(`{"apiVersion":"client.authentication.k8s.io/v1beta1","kind":"ExecCredential","status":{%s}}`, status)
		os.Exit(0)
	case 204, 404, 401, 403:
		os.Exit(0)
	default:
		os.Exit(1)
	}
}
