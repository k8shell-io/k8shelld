// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package models

import (
	"fmt"

	"github.com/k8shell-io/common/pkg/models"
)

// Group represents a supplementary group.
type Group struct {
	Name string
	Gid  int
}

// User holds the workspace identity. It is populated once from the user's
// profile in NewUser and never changes for the lifetime of the process — all
// fields may be read from any goroutine without synchronization.
type User struct {
	profile models.UserProfile
	homeDir string
	groups  []Group
}

// NewUser creates a User from a resolved profile.
func NewUser(profile *models.UserProfile) *User {
	return &User{profile: *profile}
}

// String returns a human-readable representation for logging.
func (u *User) String() string {
	return fmt.Sprintf(
		"User{Username: %s, UID: %d, GID: %d, Name: %s, Email: %s, Shell: %s, Sudo: %t, Roles: %v}",
		u.profile.Username, u.profile.UID, u.profile.GID,
		u.profile.Fullname, u.profile.Email, u.GetShell(), u.profile.Sudo, u.profile.Roles,
	)
}

// HasRole checks if the user has a specific role.
func (u *User) HasRole(role models.Role) bool {
	for _, r := range u.profile.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// GetShell returns the login shell, defaulting to /bin/sh when unset.
func (u *User) GetShell() string {
	if u.profile.Shell != "" {
		return u.profile.Shell
	}
	return "/bin/sh"
}

// SudoEnabled returns whether passwordless sudo is enabled for the user.
func (u *User) SudoEnabled() bool {
	return u.profile.Sudo
}

// ProfileSnapshot returns a copy of the user's profile.
func (u *User) ProfileSnapshot() models.UserProfile {
	return u.profile
}

// GetUsername returns the username.
func (u *User) GetUsername() string {
	return u.profile.Username
}

// GetUID returns the user's UID.
func (u *User) GetUID() uint32 {
	return u.profile.UID
}

// GetGID returns the user's primary GID.
func (u *User) GetGID() uint32 {
	return u.profile.GID
}

// GetHomeDir returns the home directory, defaulting to /home/<username> when unset.
func (u *User) GetHomeDir() string {
	if u.homeDir != "" {
		return u.homeDir
	}
	return "/home/" + u.profile.Username
}

// GetGroups returns the supplementary groups for the user.
// TODO: derive from the profile or a policy source once that is implemented.
func (u *User) GetGroups() []Group {
	return u.groups
}

// ShellUser is an immutable snapshot of the identity fields needed to launch a
// shell process.  Obtain it via NewShellUser before starting a session; the
// snapshot is safe to use without further locking for the session's lifetime.
type ShellUser struct {
	Username string
	UID      uint32
	GID      uint32
	HomeDir  string
	Shell    string
	Sudo     bool
	Groups   []Group
}

// NewShellUser takes a snapshot of User for use in a shell session.
func NewShellUser(u *User) ShellUser {
	uid := u.profile.UID
	if uid == 0 {
		uid = 1000
	}
	gid := u.profile.GID
	if gid == 0 {
		gid = 1000
	}

	return ShellUser{
		Username: u.profile.Username,
		UID:      uid,
		GID:      gid,
		HomeDir:  u.GetHomeDir(),
		Shell:    u.GetShell(),
		Sudo:     u.profile.Sudo,
		Groups:   u.groups,
	}
}
