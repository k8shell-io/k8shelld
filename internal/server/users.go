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
	"time"

	"github.com/k8shell-io/k8shelld/internal/log"
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
	Shell    string `yaml:"shell"`
	Sudo     bool   `yaml:"sudo"`
	HomeDir  string
	Groups   *[]Group `yaml:"groups,omitempty" json:"groups,omitempty"`
}

const groupFilePath = "/etc/group"

func runCommand(ctx context.Context, cmd *exec.Cmd) ([]byte, error) {
	cmd = exec.CommandContext(ctx, cmd.Path, cmd.Args[1:]...)
	return cmd.CombinedOutput()
}

// CreateUser creates the user in the system.
func CreateUser(user User) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	logger := log.NewLogger("user-management")
	logger.Info().Msgf("Main user: username=%s, uid=%d, gid=%d, home=%s, shell=%s, sudo=%t, groups=%v",
		user.Username, user.Uid, user.Gid, user.HomeDir, user.Shell, user.Sudo, user.Groups)

	// Check if the main group exists, and create it if it doesn't
	if exists, err := groupExists(strconv.Itoa(user.Gid)); err != nil {
		return fmt.Errorf("failed to check main group: %v", err)
	} else if !exists {
		if err := addGroup(ctx, user.Username, user.Gid); err != nil {
			return fmt.Errorf("failed to add the user main group: %v", err)
		}
		logger.Info().Msgf("Main group created: %s (%d)", user.Username, user.Gid)
	}

	// Check if the user exists, and create it if it doesn't
	if exists, err := userExists(strconv.Itoa(user.Uid)); err != nil {
		return fmt.Errorf("failed to check main user: %v", err)
	} else if !exists {
		if err := addUser(ctx, user.Username, user.Uid, user.Gid,
			fmt.Sprintf("/home/%s", user.Username), user.Shell); err != nil {
			return fmt.Errorf("failed to add user: %v", err)
		}
		logger.Info().Msgf("Main user created: %s (%d)", user.Username, user.Uid)
	}

	// Add the user to the specified groups
	if user.Groups != nil && len(*user.Groups) > 0 {
		for _, group := range *user.Groups {
			if exists, err := groupExists(strconv.Itoa(group.Gid)); err != nil {
				return fmt.Errorf("failed to check group %v: %v", group, err)
			} else if !exists {
				if err := addGroup(ctx, group.Name, group.Gid); err != nil {
					return fmt.Errorf("failed to create group %v: %v", group, err)
				}
				logger.Debug().Msgf("Group created: %v", group)
			}
			cmd := exec.CommandContext(ctx, "usermod", "-aG", strconv.Itoa(group.Gid), user.Username)
			output, err := runCommand(ctx, cmd)
			if err != nil {
				return fmt.Errorf("failed to add user %s to group %v: %v, output: %s", user.Username, group, err, string(output))
			}
			logger.Debug().Msgf("User %s added to group %v", user.Username, group)
		}
	}

	// Copy skeleton files to the main user's home directory
	if err := copySkeletonFiles(ctx, user.Uid, user.Gid, user.HomeDir); err != nil {
		return fmt.Errorf("failed to copy skeleton files: %v", err)
	}

	// Enable passwordless sudo for the main user
	if user.Sudo {
		if err := enablePasswordlessSudo(ctx, user.Username); err != nil {
			logger.Error().Msgf("Failed to enable passwordless sudo for user %s: %v", user.Username, err)
		}
	}

	return nil
}

// groupExists checks if a group with the given name or GID exists.
func groupExists(nameOrGID string) (bool, error) {
	data, err := os.ReadFile(groupFilePath)
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
func userExists(nameOrUID string) (bool, error) {
	if _, err := user.Lookup(nameOrUID); err == nil {
		return true, nil
	}
	if _, err := user.LookupId(nameOrUID); err == nil {
		return true, nil
	}
	return false, nil
}

// createGroup creates a group with the given name and GID.
func addGroup(ctx context.Context, groupName string, gid int) error {
	cmd := exec.CommandContext(ctx, "groupadd", "-g", fmt.Sprintf("%d", gid), groupName)
	output, err := runCommand(ctx, cmd)
	if err != nil {
		return fmt.Errorf("failed to create group %s with GID %d: %v, output: %s", groupName, gid, err, string(output))
	}
	return nil
}

// createUser creates a user with the given username, UID, GID, home directory, and shell.
func addUser(ctx context.Context, username string, uid, gid int, homeDir, shell string) error {
	// Create the user
	cmd := exec.CommandContext(ctx, "useradd", "-u", fmt.Sprintf("%d", uid), "-g", fmt.Sprintf("%d", gid),
		"-d", homeDir, "-s", shell, "-m", username)
	output, err := runCommand(ctx, cmd)
	if err != nil {
		return fmt.Errorf("failed to create user: %s, error: %v", string(output), err)
	}

	// Change the ownership of the home directory to the user
	cmd = exec.CommandContext(ctx, "chown", fmt.Sprintf("%d:%d", uid, gid), homeDir)
	if _, err := runCommand(ctx, cmd); err != nil {
		return fmt.Errorf("failed to change ownership of home directory %s: %v", homeDir, err)
	}

	// Change the permissions of the home directory
	cmd = exec.CommandContext(ctx, "chmod", "700", homeDir)
	if _, err := runCommand(ctx, cmd); err != nil {
		return fmt.Errorf("failed to change permissions of home directory %s: %v", homeDir, err)
	}
	return nil
}

// enablePasswordlessSudo enables passwordless sudo for the given user.
func enablePasswordlessSudo(ctx context.Context, username string) error {
	sudoersFile := filepath.Join("/etc/sudoers.d", username)
	content := fmt.Sprintf("%s ALL=(ALL) NOPASSWD:ALL\n", username)
	tmpFile := sudoersFile + ".tmp"
	if err := os.WriteFile(tmpFile, []byte(content), 0440); err != nil {
		return fmt.Errorf("failed to write sudoers temp file for %s: %v", username, err)
	}
	cmd := exec.Command("visudo", "-c", "-f", tmpFile)
	_, err := runCommand(ctx, cmd)
	if err != nil {
		return fmt.Errorf("sudoers file validation failed for %s", username)
	}
	return os.Rename(tmpFile, sudoersFile)
}

// getUserLoginShell returns the login shell for the given user.
func getUserLoginShell(username string) (string, error) {
	data, err := os.ReadFile("/etc/passwd")
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
func copySkeletonFiles(ctx context.Context, uid, gid int, homeDir string) error {
	if _, err := os.Stat(homeDir); os.IsNotExist(err) {
		if err := os.MkdirAll(homeDir, 0755); err != nil {
			return fmt.Errorf("failed to create home directory %s: %w", homeDir, err)
		}
	}

	if _, err := os.Stat("/etc/skel"); os.IsNotExist(err) {
		return fmt.Errorf("the /etc/skel directory does not exist: %w", err)
	}

	cmd := exec.CommandContext(ctx, "cp", "-r", "/etc/skel/.", homeDir)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{
			Uid: uint32(uid),
			Gid: uint32(gid),
		},
	}
	if _, err := runCommand(ctx, cmd); err != nil {
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
