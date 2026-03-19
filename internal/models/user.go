package models

import (
	"fmt"
	"sync"
	"time"

	"github.com/k8shell-io/common/pkg/authz"
	"github.com/k8shell-io/common/pkg/models"
)

// Group represents a supplementary group.
type Group struct {
	Name string
	Gid  int
}

// User holds the workspace identity.
//
// Immutable fields (username, uid, gid, homeDir, groups) are set once in NewUser
// and never changed — they may be read from any goroutine without acquiring mu.
//
// Mutable fields (claims, userToken) are replaced atomically on each token renewal
// and must always be accessed through the accessor methods, which acquire mu internally.
type User struct {
	mu sync.RWMutex

	// Immutable identity fields — set in NewUser, never written again.
	username string
	uid      uint32
	gid      uint32
	homeDir  string
	groups   []Group

	// Mutable — replaced atomically on token renewal; requires mu.
	claims    *authz.UserClaims
	userToken string
}

// NewUser creates a User from a verified JWT claims set and the raw token string.
func NewUser(claims *authz.UserClaims, token string) *User {
	return &User{
		username:  claims.Subject,
		uid:       claims.UID,
		gid:       claims.GID,
		claims:    claims,
		userToken: token,
	}
}

// String returns a human-readable representation for logging.
func (u *User) String() string {
	u.mu.RLock()
	defer u.mu.RUnlock()
	shell := u.claims.Shell
	if shell == "" {
		shell = "/bin/sh"
	}
	return fmt.Sprintf(
		"User{Username: %s, UID: %d, GID: %d, Name: %s, Email: %s, Shell: %s, Sudo: %t, Roles: %v, Exp: %s}",
		u.username, u.uid, u.gid,
		u.claims.Name, u.claims.Email, shell, u.claims.Sudo, u.claims.Roles,
		u.claims.ExpiresAt.Time.UTC().Format(time.RFC3339),
	)
}

// Update atomically replaces the mutable JWT claims and token string.
// Returns (true, nil) when the update was applied, (false, nil) when the token
// is unchanged (no-op), or (false, error) when an immutable field has changed.
func (u *User) Update(claims *authz.UserClaims, token string) (bool, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	if token == u.userToken {
		return false, nil // same token — nothing to do
	}

	// Validate immutable fields before making any change.
	if claims.Subject != u.username {
		return false, fmt.Errorf("cannot update user subject from %s to %s", u.username, claims.Subject)
	}
	if claims.UID != u.uid || claims.GID != u.gid {
		return false, fmt.Errorf("cannot update user UID/GID from %d/%d to %d/%d", u.uid, u.gid, claims.UID, claims.GID)
	}

	u.claims = claims
	u.userToken = token
	return true, nil
}

// HasRole checks if the user has a specific role.
func (u *User) HasRole(role models.Role) bool {
	u.mu.RLock()
	defer u.mu.RUnlock()
	for _, r := range u.claims.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// GetShell returns the login shell, defaulting to /bin/sh when unset.
func (u *User) GetShell() string {
	u.mu.RLock()
	defer u.mu.RUnlock()
	if u.claims.Shell != "" {
		return u.claims.Shell
	}
	return "/bin/sh"
}

// SudoEnabled returns whether passwordless sudo is enabled for the user.
func (u *User) SudoEnabled() bool {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return u.claims.Sudo
}

// ClaimsSnapshot returns a copy of the current JWT claims under the read lock.
// The returned value is safe to inspect without any further locking.
func (u *User) ClaimsSnapshot() authz.UserClaims {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return *u.claims
}

// GetUserToken returns the current raw JWT string.
func (u *User) GetUserToken() string {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return u.userToken
}

// GetUsername returns the username (JWT subject). Immutable — no lock needed.
func (u *User) GetUsername() string {
	return u.username
}

// GetUID returns the user's UID. Immutable — no lock needed.
func (u *User) GetUID() uint32 {
	return u.uid
}

// GetGID returns the user's primary GID. Immutable — no lock needed.
func (u *User) GetGID() uint32 {
	return u.gid
}

// GetHomeDir returns the home directory, defaulting to /home/<username> when unset.
// Immutable — no lock needed.
func (u *User) GetHomeDir() string {
	if u.homeDir != "" {
		return u.homeDir
	}
	return "/home/" + u.username
}

// GetGroups returns the supplementary groups for the user.
// TODO: derive from JWT claims or a policy source once that is implemented.
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

// NewShellUser takes an atomic snapshot of User for use in a shell session.
func NewShellUser(u *User) ShellUser {
	// Immutable fields — read without lock.
	uid := u.uid
	if uid == 0 {
		uid = 1000
	}
	gid := u.gid
	if gid == 0 {
		gid = 1000
	}

	// Mutable fields — single RLock for a consistent snapshot.
	u.mu.RLock()
	shell := u.claims.Shell
	if shell == "" {
		shell = "/bin/sh"
	}
	sudo := u.claims.Sudo
	u.mu.RUnlock()

	return ShellUser{
		Username: u.username,
		UID:      uid,
		GID:      gid,
		HomeDir:  u.GetHomeDir(),
		Shell:    shell,
		Sudo:     sudo,
		Groups:   u.groups,
	}
}
