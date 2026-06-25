// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package system

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	"github.com/k8shell-io/k8shelld/internal/logger"
	"github.com/rs/zerolog"
)

// alpineProvider implements distroProvider for Alpine Linux and BusyBox-based
// environments, which use the adduser/addgroup applets instead of shadow-utils.
type alpineProvider struct {
	log *zerolog.Logger
}

func NewAlpineProvider() *alpineProvider {
	return &alpineProvider{log: logger.NewLogger("alpineProvider")}
}

func (p *alpineProvider) addGroup(ctx context.Context, groupName string, gid int) error {
	cmd := exec.CommandContext(ctx, "addgroup", "-g", fmt.Sprintf("%d", gid), groupName)
	output, err := runCommand(ctx, cmd)
	if err != nil {
		return fmt.Errorf("failed to create group %s with GID %d: %v, output: %s", groupName, gid, err, string(output))
	}
	return nil
}

// addUser creates a user via BusyBox/Alpine adduser.
// adduser requires the primary group to be referenced by name, so we look it
// up from /etc/group using the provided GID.
func (p *alpineProvider) addUser(ctx context.Context, username string, uid, gid int, homeDir, shell string) error {
	groupName, err := groupNameByGID(gid)
	if err != nil {
		return fmt.Errorf("failed to resolve group name for GID %d: %v", gid, err)
	}

	// -D: no password (non-interactive), -H: don't auto-create home here so we
	// can apply the right ownership ourselves after creation.
	cmd := exec.CommandContext(ctx, "adduser",
		"-D",
		"-H",
		"-u", fmt.Sprintf("%d", uid),
		"-G", groupName,
		"-h", homeDir,
		"-s", shell,
		username,
	)
	output, err := runCommand(ctx, cmd)
	if err != nil {
		return fmt.Errorf("failed to create user: %s, error: %v", string(output), err)
	}

	// Create the home directory with the correct ownership and permissions.
	if err := os.MkdirAll(homeDir, 0700); err != nil {
		return fmt.Errorf("failed to create home directory %s: %v", homeDir, err)
	}

	if err = os.Chown(homeDir, uid, gid); err != nil {
		p.log.Error().Err(err).Msgf("Failed to change ownership of home directory %s to UID %d and GID %d: %v", homeDir, uid, gid, err)
	}

	if err := os.Chmod(homeDir, 0700); err != nil {
		p.log.Error().Err(err).Msgf("Failed to set permissions of home directory %s to 0700: %v", homeDir, err)
	}
	return nil
}

// addUserToGroup uses addgroup's "addgroup USER GROUP" form which takes group name.
// The groupName parameter is preferred; we fall back to a GID lookup only when it
// is empty (e.g. future callers that do not supply the name).
func (p *alpineProvider) addUserToGroup(ctx context.Context, username, groupName string, gid int) error {
	name := groupName
	if name == "" {
		var err error
		name, err = groupNameByGID(gid)
		if err != nil {
			return fmt.Errorf("failed to resolve group name for GID %d: %v", gid, err)
		}
	}
	cmd := exec.CommandContext(ctx, "addgroup", username, name)
	output, err := runCommand(ctx, cmd)
	if err != nil {
		return fmt.Errorf("failed to add user %s to group %s: %v, output: %s", username, name, err, string(output))
	}
	return nil
}
