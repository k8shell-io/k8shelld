// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

// Package apiclient wraps the k8shell-go SDK client with the internal,
// server-to-server identity operations k8shelld needs that the public SDK
// does not expose: issuing this workspace's own identity token, and updating
// the client's bearer token in place after each renewal.
package apiclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/k8shell-io/common/pkg/models"
	k8shell "github.com/k8shell-io/k8shell-go"
)

// Client wraps a k8shell-go SDK client and adds the internal token-issuance
// endpoint plus in-place token renewal, neither of which the public SDK
// exposes.
type Client struct {
	server string
	http   *http.Client

	mu    sync.RWMutex
	token string
	sdk   *k8shell.Client
}

// New creates a Client for the given API server address with no token set.
// Call UpdateToken once an identity token has been issued.
func New(server string) *Client {
	server = strings.TrimSuffix(server, "/")
	return &Client{
		server: server,
		http:   &http.Client{Timeout: 5 * time.Second},
		sdk:    k8shell.New(server, ""),
	}
}

// UpdateToken replaces the bearer token used for all subsequent requests,
// including the SDK client used for delegated calls.
func (c *Client) UpdateToken(token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token = token
	c.sdk = k8shell.New(c.server, token)
}

func (c *Client) current() (*k8shell.Client, string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.sdk, c.token
}

// IssueUserToken requests a fresh identity JWT for username from the API
// server's internal, workspace-only token endpoint. This is a
// server-to-server operation with no equivalent in the public k8shell-go SDK.
func (c *Client) IssueUserToken(ctx context.Context, username string) (string, error) {
	_, token := c.current()

	endpoint := fmt.Sprintf("%s/api/v1/internal/users/%s/token", c.server, username)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return "", err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("issue user token: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read issue-token response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("issue user token: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("decode issue-token response: %w", err)
	}
	return out.Token, nil
}

// ListSessions delegates to the underlying SDK client's session listing.
func (c *Client) ListSessions(ctx context.Context, username, workspace string, limit int, all bool) ([]models.SSHSession, error) {
	sdk, _ := c.current()
	return sdk.ListSessions(ctx, username, workspace, limit, all)
}

// ResolveUserCredential delegates to the underlying SDK client's credential resolution.
func (c *Client) ResolveUserCredential(ctx context.Context, username, serviceName, scope string) (*models.UserCredential, error) {
	sdk, _ := c.current()
	return sdk.ResolveUserCredential(ctx, username, serviceName, scope)
}

// ComposeBlueprint delegates to the underlying SDK client's blueprint composition.
func (c *Client) ComposeBlueprint(ctx context.Context, username string, k8shellFile *models.K8shellFile) (*models.Blueprint, error) {
	sdk, _ := c.current()
	return sdk.ComposeBlueprint(ctx, username, k8shellFile)
}
