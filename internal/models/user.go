package models

// Group represents a group in the workspace
type Group struct {
	Name string `yaml:"name"`
	Gid  int    `yaml:"gid"`
}

// User represents a user in the workspace
type User struct {
	Username  string   `yaml:"username"`
	Fullname  string   `yaml:"fullname"`
	Uid       uint32   `yaml:"uid"`
	Gid       uint32   `yaml:"gid"`
	Shell     string   `yaml:"shell"`
	Sudo      bool     `yaml:"sudo"`
	Groups    *[]Group `yaml:"groups,omitempty" json:"groups,omitempty"`
	HomeDir   string
	UserToken string
}
