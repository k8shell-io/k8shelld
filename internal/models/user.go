package models

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
	Username  string
	Fullname  string
	Uid       uint32
	Gid       uint32
	Shell     string  // not from JWT; use GetShell() for a default
	Sudo      bool    // not from JWT; defaults to false
	Groups    []Group // not from JWT; use GetGroups() — stub until implemented
	HomeDir   string  // not from JWT; use GetHomeDir() for a default
	UserToken string
}

// GetShell returns the login shell, defaulting to /bin/bash when unset.
func (u User) GetShell() string {
	if u.Shell != "" {
		return u.Shell
	}
	return "/bin/bash"
}

// GetHomeDir returns the home directory, defaulting to /home/<username> when unset.
func (u User) GetHomeDir() string {
	if u.HomeDir != "" {
		return u.HomeDir
	}
	return "/home/" + u.Username
}

// GetGroups returns the supplementary groups for the user.
// TODO: derive from JWT claims or a policy source once that is implemented.
func (u User) GetGroups() []Group {
	return u.Groups
}
