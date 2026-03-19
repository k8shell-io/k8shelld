package system

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

	"github.com/k8shell-io/k8shelld/internal/logger"
	"github.com/k8shell-io/k8shelld/internal/models"
	"github.com/k8shell-io/k8shelld/internal/utils"
)

const groupFilePath = "/etc/group"

// distroProvider abstracts the OS-level user/group management commands that
// differ across Linux distributions
type distroProvider interface {
	// addGroup creates a new group with the given name and GID.
	addGroup(ctx context.Context, groupName string, gid int) error

	// addUser creates a new user with the given attributes, ensures the home
	// directory exists and applies the correct ownership + permissions.
	addUser(ctx context.Context, username string, uid, gid int, homeDir, shell string) error

	// addUserToGroup adds username to the supplemental group identified by
	// groupName (preferred) or gid (fallback).
	addUserToGroup(ctx context.Context, username, groupName string, gid int) error
}

// getProvider detects the running environment and returns the appropriate distroProvider implementation
//
//   - Alpine Linux is detected via /etc/alpine-release
//   - BusyBox-based images are detected by the absence of useradd and the presence of adduser
//   - All other systems (Ubuntu, Debian, CentOS, RHEL …) use the standard shadow-utils provider
func getProvider() distroProvider {
	// Alpine Linux always ships /etc/alpine-release
	if _, err := os.Stat("/etc/alpine-release"); err == nil {
		return NewAlpineProvider()
	}
	// Generic BusyBox image without /etc/alpine-release
	if _, err := exec.LookPath("useradd"); err != nil {
		if _, err2 := exec.LookPath("adduser"); err2 == nil {
			return NewAlpineProvider()
		}
	}
	return NewStandardProvider()
}

// runCommand executes cmd under the supplied context and returns the combined
// stdout+stderr output.  Any SysProcAttr set on cmd is forwarded to the new
// context-aware command so that credential overrides are honoured
func runCommand(ctx context.Context, cmd *exec.Cmd) ([]byte, error) {
	ctxCmd := exec.CommandContext(ctx, cmd.Path, cmd.Args[1:]...)
	ctxCmd.SysProcAttr = cmd.SysProcAttr
	return ctxCmd.CombinedOutput()
}

// CreateUser creates the user in the system.
func CreateUser(user *models.User) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	log := logger.NewLogger("user-management")
	log.Info().Msgf("Main user: username=%s, uid=%d, gid=%d, home=%s, shell=%s, sudo=%t, groups=%v",
		user.GetUsername(), user.UID, user.GID, user.GetHomeDir(), user.GetShell(), user.SudoEnabled(), user.GetGroups())

	provider := getProvider()

	// Check if the main group exists, and create it if it doesn't
	if exists, err := groupExists(strconv.Itoa(int(user.GID))); err != nil {
		return fmt.Errorf("failed to check main group: %v", err)
	} else if !exists {
		if err := provider.addGroup(ctx, user.GetUsername(), int(user.GID)); err != nil {
			return fmt.Errorf("failed to add the user main group: %v", err)
		}
		log.Info().Msgf("Main group created: %s (%d)", user.GetUsername(), user.GID)
	}

	// Check if the user exists, and create it if it doesn't
	if u := UserExists(strconv.Itoa(int(user.UID))); u == nil {
		if err := provider.addUser(ctx, user.GetUsername(), int(user.UID), int(user.GID), user.GetHomeDir(), user.GetShell()); err != nil {
			return fmt.Errorf("failed to add user: %v", err)
		}
		log.Info().Msgf("Main user created: %s (%d)", user.GetUsername(), user.UID)
	}

	// Add the user to the specified groups
	for _, group := range user.GetGroups() {
		// GID is authoritative: check by GID first.
		resolvedName := group.Name
		if exists, err := groupExists(strconv.Itoa(int(group.Gid))); err != nil {
			return fmt.Errorf("failed to check group %v: %v", group, err)
		} else if !exists {
			if err := provider.addGroup(ctx, group.Name, int(group.Gid)); err != nil {
				return fmt.Errorf("failed to create group %v: %v", group, err)
			}
			log.Debug().Msgf("Group created: %v", group)
		} else {
			// Group already exists under this GID; resolve its actual name
			// (may differ from the requested name) so Alpine's addgroup can
			// reference it correctly.
			if actual, err := groupNameByGID(int(group.Gid)); err == nil {
				resolvedName = actual
				log.Debug().Msgf("Group with GID %d already exists as %s; using existing group",
					group.Gid, actual)
			}
		}
		if err := provider.addUserToGroup(ctx, user.GetUsername(), resolvedName, int(group.Gid)); err != nil {
			return fmt.Errorf("failed to add user %s to group %v: %v", user.GetUsername(), group, err)
		}
		log.Debug().Msgf("User %s added to group %v", user.GetUsername(), group)
	}

	// Copy skeleton files to the main user's home directory
	if err := copySkeletonFiles(ctx, int(user.UID), int(user.GID), user.GetHomeDir()); err != nil {
		return fmt.Errorf("failed to copy skeleton files: %v", err)
	}

	// Enable passwordless sudo for the main user
	if user.SudoEnabled() {
		if err := enablePasswordlessSudo(ctx, user.GetUsername()); err != nil {
			log.Error().Msgf("Failed to enable passwordless sudo for user %s: %v", user.GetUsername(), err)
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
func UserExists(nameOrUID string) *user.User {
	if u, err := user.Lookup(nameOrUID); err == nil {
		return u
	}
	if u, err := user.LookupId(nameOrUID); err == nil {
		return u
	}
	return nil
}

// groupNameByGID returns the group name for the given GID from /etc/group.
func groupNameByGID(gid int) (string, error) {
	data, err := os.ReadFile(groupFilePath)
	if err != nil {
		return "", fmt.Errorf("failed to read %s: %v", groupFilePath, err)
	}
	target := strconv.Itoa(gid)
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		parts := strings.Split(scanner.Text(), ":")
		if len(parts) >= 3 && parts[2] == target {
			return parts[0], nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("error scanning %s: %v", groupFilePath, err)
	}
	return "", fmt.Errorf("group with GID %d not found in %s", gid, groupFilePath)
}

// enablePasswordlessSudo enables passwordless sudo for the given user.
func enablePasswordlessSudo(ctx context.Context, username string) error {
	sudoersFile := filepath.Join("/etc/sudoers.d", username)
	content := fmt.Sprintf("%s ALL=(ALL) NOPASSWD:ALL\n", username)
	tmpFile := sudoersFile + ".tmp"
	if err := os.WriteFile(tmpFile, []byte(content), 0440); err != nil {
		return fmt.Errorf("failed to write sudoers temp file for %s: %v", username, err)
	}

	// Validate with visudo when available; skip on minimal images (e.g. Alpine)
	// that may not have it installed.
	if visudoPath, err := exec.LookPath("visudo"); err == nil {
		cmd := exec.Command(visudoPath, "-c", "-f", tmpFile)
		if _, err := runCommand(ctx, cmd); err != nil {
			_ = os.Remove(tmpFile)
			return fmt.Errorf("sudoers file validation failed for %s", username)
		}
	}
	return os.Rename(tmpFile, sudoersFile)
}

// GetUserLoginShell returns the login shell for the given user.
func GetUserLoginShell(username string) (string, error) {
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
		// /etc/skel is absent (common on Alpine/BusyBox); skip skeleton copy.
		return nil
	}

	cmd := exec.CommandContext(ctx, "cp", "-r", "/etc/skel/.", homeDir)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{
			Uid: utils.SafeIntToUint32(uid),
			Gid: utils.SafeIntToUint32(gid),
		},
	}
	if _, err := runCommand(ctx, cmd); err != nil {
		return fmt.Errorf("failed to copy skeleton files: %w", err)
	}
	return nil
}

func GetSupplementalGroups(username string) []uint32 {
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
			gids = append(gids, utils.SafeIntToUint32(gidInt))
		}
	}
	return gids
}
