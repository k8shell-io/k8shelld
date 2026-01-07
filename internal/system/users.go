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
	"github.com/k8shell-io/k8shelld/internal/types"
)

const groupFilePath = "/etc/group"

func isValidUnixUsername(username string) bool {
	// Conservative validation to prevent path traversal and weird sudoers filenames.
	// Matches typical Linux username rules: [a-z_][a-z0-9_-]*[$]?
	if username == "" {
		return false
	}
	if filepath.Base(username) != username {
		return false
	}
	if strings.ContainsRune(username, 0) {
		return false
	}

	// Optional trailing '$' for system users.
	name := username
	if strings.HasSuffix(name, "$") {
		name = strings.TrimSuffix(name, "$")
		if name == "" {
			return false
		}
	}

	first := name[0]
	if !((first >= 'a' && first <= 'z') || first == '_') {
		return false
	}
	for i := 1; i < len(name); i++ {
		c := name[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '-' {
			continue
		}
		return false
	}
	return true
}

func isValidUnixGroupName(groupName string) bool {
	// Conservative validation for group names: [a-z_][a-z0-9_-]*
	if groupName == "" {
		return false
	}
	if filepath.Base(groupName) != groupName {
		return false
	}
	if strings.ContainsRune(groupName, 0) {
		return false
	}

	first := groupName[0]
	if !((first >= 'a' && first <= 'z') || first == '_') {
		return false
	}
	for i := 1; i < len(groupName); i++ {
		c := groupName[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '-' {
			continue
		}
		return false
	}
	return true
}

func isSafeAbsPath(p string) bool {
	if p == "" {
		return false
	}
	if strings.ContainsRune(p, 0) {
		return false
	}
	if !filepath.IsAbs(p) {
		return false
	}
	return filepath.Clean(p) == p
}

func runCommand(_ context.Context, cmd *exec.Cmd) ([]byte, error) {
	return cmd.CombinedOutput()
}

// CreateUser creates the user in the system.
func CreateUser(user types.User) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	logger := logger.NewLogger("user-management")

	if !isValidUnixUsername(user.Username) {
		return fmt.Errorf("invalid username: %q", user.Username)
	}

	homeDir := user.HomeDir
	if homeDir == "" {
		homeDir = fmt.Sprintf("/home/%s", user.Username)
	}
	if !isSafeAbsPath(homeDir) {
		return fmt.Errorf("invalid home directory path: %q", homeDir)
	}
	if user.Shell != "" && !isSafeAbsPath(user.Shell) {
		return fmt.Errorf("invalid shell path: %q", user.Shell)
	}

	logger.Info().Msgf("Main user: username=%s, uid=%d, gid=%d, home=%s, shell=%s, sudo=%t, groups=%v",
		user.Username, user.Uid, user.Gid, homeDir, user.Shell, user.Sudo, user.Groups)

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
			homeDir, user.Shell); err != nil {
			return fmt.Errorf("failed to add user: %v", err)
		}
		logger.Info().Msgf("Main user created: %s (%d)", user.Username, user.Uid)
	}

	// Add the user to the specified groups
	if user.Groups != nil && len(*user.Groups) > 0 {
		for _, group := range *user.Groups {
			if !isValidUnixGroupName(group.Name) {
				return fmt.Errorf("invalid group name: %q", group.Name)
			}

			if exists, err := groupExists(strconv.Itoa(group.Gid)); err != nil {
				return fmt.Errorf("failed to check group %v: %v", group, err)
			} else if !exists {
				if err := addGroup(ctx, group.Name, group.Gid); err != nil {
					return fmt.Errorf("failed to create group %v: %v", group, err)
				}
				logger.Debug().Msgf("Group created: %v", group)
			}
			// #nosec G204 -- user.Username is validated; group.Gid is an integer converted to string; no shell is invoked.
			cmd := exec.CommandContext(ctx, "usermod", "-aG", strconv.Itoa(group.Gid), user.Username)
			output, err := runCommand(ctx, cmd)
			if err != nil {
				return fmt.Errorf("failed to add user %s to group %v: %v, output: %s", user.Username, group, err, string(output))
			}
			logger.Debug().Msgf("User %s added to group %v", user.Username, group)
		}
	}

	// Copy skeleton files to the main user's home directory
	if err := copySkeletonFiles(ctx, user.Uid, user.Gid, homeDir); err != nil {
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
	if !isValidUnixGroupName(groupName) {
		return fmt.Errorf("invalid group name: %q", groupName)
	}
	if gid < 0 {
		return fmt.Errorf("invalid gid: %d", gid)
	}

	// #nosec G204 -- groupName is validated; gid is an integer; no shell is invoked.
	cmd := exec.CommandContext(ctx, "groupadd", "-g", strconv.Itoa(gid), groupName)
	output, err := runCommand(ctx, cmd)
	if err != nil {
		return fmt.Errorf("failed to create group %s with GID %d: %v, output: %s", groupName, gid, err, string(output))
	}
	return nil
}

// createUser creates a user with the given username, UID, GID, home directory, and shell.
func addUser(ctx context.Context, username string, uid, gid int, homeDir, shell string) error {
	if !isValidUnixUsername(username) {
		return fmt.Errorf("invalid username: %q", username)
	}
	if uid < 0 {
		return fmt.Errorf("invalid uid: %d", uid)
	}
	if gid < 0 {
		return fmt.Errorf("invalid gid: %d", gid)
	}
	if !isSafeAbsPath(homeDir) {
		return fmt.Errorf("invalid home directory path: %q", homeDir)
	}
	if shell != "" && !isSafeAbsPath(shell) {
		return fmt.Errorf("invalid shell path: %q", shell)
	}

	// Create the user
	// #nosec G204 -- username is validated; uid/gid are integers; homeDir/shell are validated absolute paths; no shell is invoked.
	cmd := exec.CommandContext(ctx, "useradd", "-u", strconv.Itoa(uid), "-g", strconv.Itoa(gid),
		"-d", homeDir, "-s", shell, "-m", username)
	output, err := runCommand(ctx, cmd)
	if err != nil {
		return fmt.Errorf("failed to create user: %s, error: %v", string(output), err)
	}

	// Change the ownership of the home directory to the user
	if err := os.Chown(homeDir, uid, gid); err != nil {
		return fmt.Errorf("chown %s: %w", homeDir, err)
	}

	// Change the permissions of the home directory
	// #nosec G204 -- homeDir is a validated absolute path; no shell is invoked.
	cmd = exec.CommandContext(ctx, "chmod", "700", homeDir)
	if _, err := runCommand(ctx, cmd); err != nil {
		return fmt.Errorf("failed to change permissions of home directory %s: %v", homeDir, err)
	}
	return nil
}

// enablePasswordlessSudo enables passwordless sudo for the given user.
func enablePasswordlessSudo(ctx context.Context, username string) error {
	if !isValidUnixUsername(username) {
		return fmt.Errorf("invalid username for sudoers: %q", username)
	}

	sudoersFile := filepath.Join("/etc/sudoers.d", username)
	content := fmt.Sprintf("%s ALL=(ALL) NOPASSWD:ALL\n", username)

	f, err := os.CreateTemp(filepath.Dir(sudoersFile), username+".tmp-")
	if err != nil {
		return fmt.Errorf("failed to create sudoers temp file for %s: %w", username, err)
	}
	tmpFile := f.Name()
	defer func() { _ = os.Remove(tmpFile) }()

	// Write as 0600 first; set sudoers perms (0440) only after validation passes
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return fmt.Errorf("failed to chmod sudoers temp file for %s: %w", username, err)
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		return fmt.Errorf("failed to write sudoers temp file for %s: %w", username, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("failed to close sudoers temp file for %s: %w", username, err)
	}

	// #nosec G204 -- tmpFile is a safe CreateTemp path under /etc/sudoers.d; username validated; no shell.
	cmd := exec.CommandContext(ctx, "visudo", "-c", "-f", tmpFile)
	output, err := runCommand(ctx, cmd)
	if err != nil {
		return fmt.Errorf("sudoers file validation failed for %s: %w (output: %s)", username, err, strings.TrimSpace(string(output)))
	}

	if err := os.Chmod(tmpFile, 0o440); err != nil {
		return fmt.Errorf("failed to chmod sudoers temp file for %s: %w", username, err)
	}
	if err := os.Rename(tmpFile, sudoersFile); err != nil {
		return fmt.Errorf("failed to install sudoers file for %s: %w", username, err)
	}
	return nil
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
	if !isSafeAbsPath(homeDir) {
		return fmt.Errorf("invalid home directory path: %q", homeDir)
	}

	if _, err := os.Stat(homeDir); os.IsNotExist(err) {
		if err := os.MkdirAll(homeDir, 0755); err != nil {
			return fmt.Errorf("failed to create home directory %s: %w", homeDir, err)
		}
	}

	if _, err := os.Stat("/etc/skel"); os.IsNotExist(err) {
		return fmt.Errorf("the /etc/skel directory does not exist: %w", err)
	}

	// #nosec G204 -- homeDir is a validated absolute path; cp args are fixed; no shell is invoked.
	cmd := exec.CommandContext(ctx, "cp", "-r", "/etc/skel/.", homeDir)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{
			Uid: SafeIntToUint32(uid),
			Gid: SafeIntToUint32(gid),
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
			gids = append(gids, SafeIntToUint32(gidInt))
		}
	}
	return gids
}
