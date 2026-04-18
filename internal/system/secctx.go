package system

import (
	"fmt"
	"os"
	"strings"
)

// SecurityContextError is returned when the container's security context is
// incompatible with k8shelld's initialization requirements.
type SecurityContextError struct {
	Issues []string
}

func (e *SecurityContextError) Error() string {
	return fmt.Sprintf("incompatible container security context:\n  - %s", strings.Join(e.Issues, "\n  - "))
}

// CheckSecurityContext inspects the running process's effective UID and the
// writability of /etc to detect security context settings that will cause
// k8shelld's initialization phase to fail.
//
// The following container security context settings are incompatible:
//   - runAsNonRoot / runAsUser (non-zero UID): prevents writing /etc/passwd,
//     /etc/group, /etc/sudoers.d and creating users/groups
//   - readOnlyRootFilesystem: prevents all writes to /etc
//   - dropping CAP_CHOWN: prevents chown of home directory and podman socket
//   - dropping CAP_SETUID/CAP_SETGID: prevents credential switching when
//     running init scripts and copying skeleton files
func CheckSecurityContext() error {
	var issues []string

	// Check 1: must run as root (UID 0). useradd/groupadd and /etc writes require it.
	if os.Getuid() != 0 {
		issues = append(issues, fmt.Sprintf(
			"process is running as UID %d; k8shelld requires UID 0 (root) to create users and write /etc — "+
				"remove runAsNonRoot or runAsUser from the container's securityContext", os.Getuid()))
	}

	// Check 2: /etc must be writable. Catches readOnlyRootFilesystem: true and
	// any restrictive mount that would block useradd/groupadd and sudoers writes.
	if err := probeWritable("/etc"); err != nil {
		issues = append(issues, fmt.Sprintf(
			"/etc is not writable (%v); k8shelld must write /etc/passwd, /etc/group and /etc/sudoers.d — "+
				"remove readOnlyRootFilesystem: true from the container's securityContext", err))
	}

	// Check 3: CAP_CHOWN — detected indirectly by attempting a no-op chown on a
	// file we own. Only run this check when we are root (otherwise the UID check
	// already covers the failure path).
	if os.Getuid() == 0 {
		if err := probeChown(); err != nil {
			issues = append(issues, fmt.Sprintf(
				"CAP_CHOWN appears to be missing (%v); k8shelld requires it to set ownership of the "+
					"user home directory and the podman socket — "+
					"remove CAP_CHOWN from capabilities.drop in the container's securityContext", err))
		}
	}

	if len(issues) > 0 {
		return &SecurityContextError{Issues: issues}
	}
	return nil
}

// probeWritable attempts to create and immediately remove a temporary file
// under dir to confirm the filesystem is writable.
func probeWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".k8shelld-secctx-probe-*")
	if err != nil {
		return err
	}
	f.Close()
	os.Remove(f.Name())
	return nil
}

// probeChown performs a no-op chown (root→root) on /proc/self to detect
// whether CAP_CHOWN is available without touching any real file.
func probeChown() error {
	return os.Lchown("/proc/self", 0, 0)
}
