// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

// Package apiclient wraps the k8shell-go SDK client, authenticated with a
// static personal access token read from the K8SHELL_PAT_TOKEN environment
// variable rather than a per-workspace identity JWT.
package apiclient

import (
	"context"
	"os"
	"strings"

	"github.com/k8shell-io/common/pkg/models"
	k8shell "github.com/k8shell-io/k8shell-go"
)

// PATTokenEnv is the environment variable holding the personal access token
// used to authenticate all outbound API server calls.
const PATTokenEnv = "K8SHELL_PAT_TOKEN"

// Client wraps a k8shell-go SDK client authenticated with the PAT from
// K8SHELL_PAT_TOKEN.
type Client struct {
	sdk *k8shell.Client
}

// New creates a Client for the given API server address, authenticated with
// the personal access token from the K8SHELL_PAT_TOKEN environment variable.
func New(server string) *Client {
	server = strings.TrimSuffix(server, "/")
	token := strings.TrimSpace(os.Getenv(PATTokenEnv))
	return &Client{
		sdk: k8shell.New(server, token),
	}
}

// ListSessions delegates to the underlying SDK client's session listing,
// reversing the order returned by the API server.
func (c *Client) ListSessions(ctx context.Context, username, workspace string, limit int, all bool) ([]models.SSHSession, error) {
	sessions, err := c.sdk.ListSessions(ctx, username, workspace, limit, all)
	if err != nil {
		return nil, err
	}
	for i, j := 0, len(sessions)-1; i < j; i, j = i+1, j-1 {
		sessions[i], sessions[j] = sessions[j], sessions[i]
	}
	return sessions, nil
}

// ResolveUserCredential delegates to the underlying SDK client's credential resolution.
func (c *Client) ResolveUserCredential(ctx context.Context, username, serviceName, scope string) (*models.UserCredential, error) {
	return c.sdk.ResolveUserCredential(ctx, username, serviceName, scope)
}

// ComposeBlueprint delegates to the underlying SDK client's blueprint composition.
func (c *Client) ComposeBlueprint(ctx context.Context, username string, k8shellFile *models.K8shellFile) (*models.Blueprint, error) {
	return c.sdk.ComposeBlueprint(ctx, username, k8shellFile)
}

// SetUserPassword delegates to the underlying SDK client's password update.
// currentPassword is required by the API server when a non-sudo user is
// changing their own password, and ignored otherwise; pass "" when not needed.
func (c *Client) SetUserPassword(ctx context.Context, username, password, currentPassword string) (*models.User, error) {
	return c.sdk.SetUserPassword(ctx, username, password, currentPassword)
}
