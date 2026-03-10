package system

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
)

// standardProvider implements distroProvider for standard Linux distributions
// that ship the shadow-utils suite: Ubuntu, Debian, CentOS, RHEL, etc.
type standardProvider struct{}

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

	cmd = exec.CommandContext(ctx, "chown", fmt.Sprintf("%d:%d", uid, gid), homeDir)
	if _, err := runCommand(ctx, cmd); err != nil {
		return fmt.Errorf("failed to change ownership of home directory %s: %v", homeDir, err)
	}

	cmd = exec.CommandContext(ctx, "chmod", "700", homeDir)
	if _, err := runCommand(ctx, cmd); err != nil {
		return fmt.Errorf("failed to change permissions of home directory %s: %v", homeDir, err)
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
