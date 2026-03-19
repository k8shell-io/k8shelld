package models

import (
	"fmt"

	"github.com/k8shell-io/common/pkg/authz"
	"github.com/k8shell-io/common/pkg/models"
)

// Group represents a supplementary group.
type Group struct {
	Name string
	Gid  int
}

// User holds the workspace identity.  The core fields (Username, Uid, Gid,
// Fullname, UserToken) are derived from the verified identity JWT at startup
// and are never read from config.yaml.
//
// Shell, Sudo, and Groups are not carried in the JWT.  Use the accessor
// methods below which return safe defaults; the fields can still be set
// programmatically when needed (e.g. from a future policy source).
type User struct {
	*authz.UserClaims
	Groups    []Group
	HomeDir   string
	UserToken string
}

func NewUser(claims *authz.UserClaims, token string) *User {
	return &User{
		UserClaims: claims,
		UserToken:  token,
	}
}

// Update updates the user information with new claims and token.
// It returns an error if any immutable fields have changed that would require a full re-login.
func (u User) Update(claims *authz.UserClaims, token string) error {
	u.Shell = claims.Shell
	u.Sudo = claims.Sudo
	u.Email = claims.Email
	u.Name = claims.Name
	u.Roles = claims.Roles
	u.UserToken = token

	// immutable fields that would require a full re-login if they change
	if claims.Subject != u.Subject {
		return fmt.Errorf("cannot update user subject from %s to %s", u.Subject, claims.Subject)
	}
	if u.UID != claims.UID || u.GID != claims.GID {
		return fmt.Errorf("cannot update user UID/GID from %d/%d to %d/%d", u.UID, u.GID, claims.UID, claims.GID)
	}

	return nil
}

// HasRole checks if the user has a specific role.
func (u User) HasRole(role models.Role) bool {
	for _, r := range u.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// GetShell returns the login shell, defaulting to /bin/bash when unset.
func (u User) GetShell() string {
	if u.Shell != "" {
		return u.Shell
	}
	return "/bin/sh"
}

// SudoEnabled returns whether passwordless sudo is enabled for the user.
func (u User) SudoEnabled() bool {
	return u.Sudo
}

// GetUsername returns the username, which is the JWT subject claim.
func (u User) GetUsername() string {
	return u.Subject
}

// GetHomeDir returns the home directory, defaulting to /home/<username> when unset.
func (u User) GetHomeDir() string {
	if u.HomeDir != "" {
		return u.HomeDir
	}
	return "/home/" + u.GetUsername()
}

// GetGroups returns the supplementary groups for the user.
// TODO: derive from JWT claims or a policy source once that is implemented.
func (u User) GetGroups() []Group {
	return u.Groups
}

type ShellUser struct {
	Username string
	UID      uint32
	GID      uint32
	HomeDir  string
	Shell    string
	Sudo     bool
	Groups   []Group
}

func NewShellUser(user *User) ShellUser {
	u := ShellUser{
		Username: user.GetUsername(),
		HomeDir:  user.GetHomeDir(),
		Shell:    user.GetShell(),
		Sudo:     user.SudoEnabled(),
		Groups:   user.GetGroups(),
	}
	if user.UID != 0 {
		u.UID = user.UID
	} else {
		u.UID = 1000
	}
	if user.GID != 0 {
		u.GID = user.GID
	} else {
		u.GID = 1000
	}
	return u
}
