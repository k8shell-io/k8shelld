package system

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"

	"github.com/k8shell-io/k8shelld/internal/logger"
	"github.com/rs/zerolog"
)

// standardProvider implements distroProvider for standard Linux distributions
// that ship the shadow-utils suite: Ubuntu, Debian, CentOS, RHEL, etc.
type standardProvider struct {
	log *zerolog.Logger
}

func NewStandardProvider() *standardProvider {
	return &standardProvider{log: logger.NewLogger("standardProvider")}
}

func (p *standardProvider) addGroup(ctx context.Context, groupName string, gid int) error {
	cmd := exec.CommandContext(ctx, "groupadd", "-g", fmt.Sprintf("%d", gid), groupName)
	output, err := runCommand(ctx, cmd)
	if err != nil {
		return fmt.Errorf("failed to create group %s with GID %d: %v, output: %s", groupName, gid, err, string(output))
	}
	return nil
}

func (p *standardProvider) addUser(ctx context.Context, username string, uid, gid int, homeDir, shell string) error {
	cmd := exec.CommandContext(ctx, "useradd",
		"-u", fmt.Sprintf("%d", uid),
		"-g", fmt.Sprintf("%d", gid),
		"-d", homeDir,
		"-s", shell,
		"-m",
		username,
	)
	output, err := runCommand(ctx, cmd)
	if err != nil {
		return fmt.Errorf("failed to create user: %s, error: %v", string(output), err)
	}

	if err = os.Chown(homeDir, uid, gid); err != nil {
		p.log.Error().Err(err).Msgf("Failed to change ownership of home directory %s to UID %d and GID %d: %v", homeDir, uid, gid, err)
	}

	if err := os.Chmod(homeDir, 0700); err != nil {
		p.log.Error().Err(err).Msgf("Failed to set permissions of home directory %s to 0700: %v", homeDir, err)
	}
	return nil
}

// addUserToGroup uses usermod which accepts GID directly.
func (p *standardProvider) addUserToGroup(ctx context.Context, username, groupName string, gid int) error {
	cmd := exec.CommandContext(ctx, "usermod", "-aG", strconv.Itoa(gid), username)
	output, err := runCommand(ctx, cmd)
	if err != nil {
		return fmt.Errorf("failed to add user %s to group %d: %v, output: %s", username, gid, err, string(output))
	}
	return nil
}
