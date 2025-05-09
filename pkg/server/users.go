package server

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

type Group struct {
	Name string `yaml:"name"`
	Gid  int    `yaml:"gid"`
}

// User represents a user in the workspace
type User struct {
	Username string `yaml:"username"`
	Fullname string `yaml:"fullname"`
	Uid      int    `yaml:"uid"`
	Gid      int    `yaml:"gid"`
}

// MainUser represents the main user in the workspace
type MainUser struct {
	User    `yaml:",inline"`
	Shell   string `yaml:"shell"`
	Sudo    bool   `yaml:"sudo"`
	HomeDir string
	Groups  []Group `yaml:"groups"`
}

const groupFilePath = "/etc/group"

// CreateMainUser creates the main user of the system.
func CreateMainUser(ctx context.Context, runner CommandRunner, fs FileSystem, lookup UserLookup,
	mainUser MainUser) error {
	logger := NewLogger("user-management")
	logger.Info("Main user: username=%s, uid=%d, gid=%d, home=%s, shell=%s, sudo=%t, groups=%v", mainUser.Username, mainUser.Uid,
		mainUser.Gid, mainUser.HomeDir, mainUser.Shell, mainUser.Sudo, mainUser.Groups)

	// Check if the main group exists, and create it if it doesn't
	if exists, err := groupExists(fs, strconv.Itoa(mainUser.Gid)); err != nil {
		return fmt.Errorf("failed to check main group: %v", err)
	} else if !exists {
		if err := createGroup(ctx, runner, mainUser.Username, mainUser.Gid); err != nil {
			return fmt.Errorf("failed to create user main group: %v", err)
		}
		logger.Info("Main group created: %s (%d)", mainUser.Username, mainUser.Gid)
	}

	// Check if the main user exists, and create it if it doesn't
	if exists, err := userExists(lookup, strconv.Itoa(mainUser.Uid)); err != nil {
		return fmt.Errorf("failed to check main user: %v", err)
	} else if !exists {
		if err := createUser(ctx, runner, mainUser.Username, mainUser.Uid, mainUser.Gid,
			fmt.Sprintf("/home/%s", mainUser.Username), mainUser.Shell); err != nil {
			return fmt.Errorf("failed to create main user: %v", err)
		}
		logger.Info("Main user created: %s (%d)", mainUser.Username, mainUser.Uid)
	}

	// Add the main user to the specified groups
	if len(mainUser.Groups) > 0 {
		for _, group := range mainUser.Groups {
			if exists, err := groupExists(fs, strconv.Itoa(group.Gid)); err != nil {
				return fmt.Errorf("failed to check group %v: %v", group, err)
			} else if !exists {
				if err := createGroup(ctx, runner, group.Name, group.Gid); err != nil {
					return fmt.Errorf("failed to create group %v: %v", group, err)
				}
				logger.Debug("Group created: %v", group)
			}
			cmd := exec.CommandContext(ctx, "usermod", "-aG", strconv.Itoa(group.Gid), mainUser.Username)
			output, err := runner.Run(ctx, cmd)
			if err != nil {
				return fmt.Errorf("failed to add user %s to group %v: %v, output: %s", mainUser.Username, group, err, string(output))
			}
			logger.Debug("User %s added to group %v", mainUser.Username, group)
		}
	}

	// Copy skeleton files to the main user's home directory
	if err := copySkeletonFiles(ctx, runner, fs, mainUser.Uid, mainUser.Gid, mainUser.HomeDir); err != nil {
		return fmt.Errorf("failed to copy skeleton files: %v", err)
	}

	// Enable passwordless sudo for the main user
	if mainUser.Sudo {
		if err := enablePasswordlessSudo(ctx, runner, fs, mainUser.Username); err != nil {
			return fmt.Errorf("error enabling passwordless sudo: %v", err)
		}
	}

	return nil
}

// groupExists checks if a group with the given name or GID exists.
func groupExists(fs FileSystem, nameOrGID string) (bool, error) {
	data, err := fs.ReadFile(groupFilePath)
	if err != nil {
		return false, fmt.Errorf("failed to read %s: %v", groupFilePath, err)
	}

	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		parts := strings.Split(scanner.Text(), ":")
		if len(parts) >= 3 && (parts[0] == nameOrGID || parts[2] == nameOrGID) {
			return true, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return false, fmt.Errorf("error scanning %s: %v", groupFilePath, err)
	}
	return false, nil
}

// userExists checks if a user with the given name or UID exists.
func userExists(lookup UserLookup, nameOrUID string) (bool, error) {
	if _, err := lookup.Lookup(nameOrUID); err == nil {
		return true, nil
	}
	if _, err := lookup.LookupId(nameOrUID); err == nil {
		return true, nil
	}
	return false, nil
}

// createGroup creates a group with the given name and GID.
func createGroup(ctx context.Context, runner CommandRunner, groupName string, gid int) error {
	cmd := exec.CommandContext(ctx, "groupadd", "-g", fmt.Sprintf("%d", gid), groupName)
	output, err := runner.Run(ctx, cmd)
	if err != nil {
		return fmt.Errorf("failed to create group %s with GID %d: %v, output: %s", groupName, gid, err, string(output))
	}
	return nil
}

// createUser creates a user with the given username, UID, GID, home directory, and shell.
func createUser(ctx context.Context, runner CommandRunner, username string, uid, gid int, homeDir, shell string) error {
	// Create the user
	cmd := exec.CommandContext(ctx, "useradd", "-u", fmt.Sprintf("%d", uid), "-g", fmt.Sprintf("%d", gid),
		"-d", homeDir, "-s", shell, "-m", username)
	output, err := runner.Run(ctx, cmd)
	if err != nil {
		return fmt.Errorf("failed to create user: %s, error: %v", string(output), err)
	}

	// Change the ownership of the home directory to the user
	cmd = exec.CommandContext(ctx, "chown", fmt.Sprintf("%d:%d", uid, gid), homeDir)
	if _, err := runner.Run(ctx, cmd); err != nil {
		return fmt.Errorf("failed to change ownership of home directory %s: %v", homeDir, err)
	}

	// Change the permissions of the home directory
	cmd = exec.CommandContext(ctx, "chmod", "700", homeDir)
	if _, err := runner.Run(ctx, cmd); err != nil {
		return fmt.Errorf("failed to change permissions of home directory %s: %v", homeDir, err)
	}
	return nil
}

// enablePasswordlessSudo enables passwordless sudo for the given user.
func enablePasswordlessSudo(ctx context.Context, runner CommandRunner, fs FileSystem, username string) error {
	sudoersFile := filepath.Join("/etc/sudoers.d", username)
	content := fmt.Sprintf("%s ALL=(ALL) NOPASSWD:ALL\n", username)
	tmpFile := sudoersFile + ".tmp"
	if err := fs.WriteFile(tmpFile, []byte(content), 0440); err != nil {
		return fmt.Errorf("failed to write sudoers temp file for %s: %v", username, err)
	}
	cmd := exec.Command("visudo", "-c", "-f", tmpFile)
	_, err := runner.Run(ctx, cmd)
	if err != nil {
		return fmt.Errorf("sudoers file validation failed for %s", username)
	}
	return fs.Rename(tmpFile, sudoersFile)
}

// getUserLoginShell returns the login shell for the given user.
func getUserLoginShell(fs FileSystem, username string) (string, error) {
	data, err := fs.ReadFile("/etc/passwd")
	if err != nil {
		return "", fmt.Errorf("failed to open /etc/passwd: %v", err)
	}

	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), ":")
		if len(fields) >= 7 && fields[0] == username {
			return fields[6], nil
		}
	}

	return "", fmt.Errorf("user %s not found", username)
}

// copySkeletonFiles copies the skeleton files to the main user's home directory.
func copySkeletonFiles(ctx context.Context, runner CommandRunner, fs FileSystem, uid, gid int, homeDir string) error {
	if _, err := fs.Stat(homeDir); os.IsNotExist(err) {
		if err := fs.MkdirAll(homeDir, 0755); err != nil {
			return fmt.Errorf("failed to create home directory %s: %w", homeDir, err)
		}
	}

	if _, err := fs.Stat("/etc/skel"); os.IsNotExist(err) {
		return fmt.Errorf("the /etc/skel directory does not exist: %w", err)
	}

	cmd := exec.CommandContext(ctx, "cp", "-r", "/etc/skel/.", homeDir)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{
			Uid: uint32(uid),
			Gid: uint32(gid),
		},
	}
	if _, err := runner.Run(ctx, cmd); err != nil {
		return fmt.Errorf("failed to copy skeleton files: %w", err)
	}
	return nil
}

func getSupplementalGroups(username string) []uint32 {
	u, err := user.Lookup(username)
	if err != nil {
		return nil
	}

	groups, err := u.GroupIds()
	if err != nil {
		return nil
	}

	var gids []uint32
	for _, gidStr := range groups {
		gidInt, err := strconv.Atoi(gidStr)
		if err == nil {
			gids = append(gids, uint32(gidInt))
		}
	}
	return gids
}
