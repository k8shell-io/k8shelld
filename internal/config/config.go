// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package config

import (
	"os"

	k8shelld "github.com/k8shell-io/common/pkg/api/client/k8shelld"
	commonmodels "github.com/k8shell-io/common/pkg/models"
	"gopkg.in/yaml.v3"
)

// Maximum packet size for streaming data
const (
	DEFAULT_MAX_PACKET_SIZE = 1 << 15 // 32 KB
)

var (
	K8SHELLD_VERSION = "0.0.0"
	K8SHELLD_COMMIT  = "0000000"
)

const (
	PODMAN_SOCKET_PATH    = "/var/run/podman/podman.sock"
	DOCKER_SOCKET_SYMLINK = "/var/run/docker.sock"
	BlueprintPath         = "/etc/k8shell/blueprint.yaml"
	ProfilePath           = "/etc/k8shell/profile.yaml"
	InitScriptsDir        = "/usr/local/k8shell/system"
)

// Type aliases for types defined in common.
type (
	// Config is now identical to k8shelld.Config. Splash, Podman, EnableApps and Apps
	// are no longer in the daemon config — they come from the blueprint.
	Config = k8shelld.Config
)

// LoadBlueprint reads and unmarshals the blueprint YAML at the given path.
// The file is expected to have the structure:
//
//	metadata: ...
//	blueprint:
//	  <blueprint fields>
func LoadBlueprint(path string) (*commonmodels.Blueprint, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var wrapper struct {
		Blueprint commonmodels.Blueprint `yaml:"blueprint"`
	}
	if err := yaml.Unmarshal(data, &wrapper); err != nil {
		return nil, err
	}
	return &wrapper.Blueprint, nil
}

// profileYAML mirrors commonmodels.UserProfile with explicit yaml tags, since
// UserProfile itself only carries json tags (it is the API server's wire type).
// Field names match those json tags (snake_case for the lock fields) since
// that's the shape the provisioner writes.
type profileYAML struct {
	Username            string              `yaml:"username"`
	Organization        string              `yaml:"organization,omitempty"`
	Fullname            string              `yaml:"fullname,omitempty"`
	Email               string              `yaml:"email,omitempty"`
	UID                 uint32              `yaml:"uid"`
	GID                 uint32              `yaml:"gid"`
	Shell               string              `yaml:"shell,omitempty"`
	Sudo                bool                `yaml:"sudo,omitempty"`
	Source              string              `yaml:"source,omitempty"`
	Roles               []commonmodels.Role `yaml:"roles,omitempty"`
	Blueprints          []string            `yaml:"blueprints,omitempty"`
	AccountLocked       bool                `yaml:"account_locked,omitempty"`
	PasswordLocked      bool                `yaml:"password_locked,omitempty"`
	PasswordLockedUntil string              `yaml:"password_locked_until,omitempty"`
}

// LoadProfile reads and unmarshals the workspace user's profile YAML at the
// given path. The file is the flat profile itself, e.g.:
//
//	username: bruckins
//	uid: 166548839
//	gid: 166548839
//	...
func LoadProfile(path string) (*commonmodels.UserProfile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p profileYAML
	if err := yaml.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	return &commonmodels.UserProfile{
		Username:            p.Username,
		Organization:        p.Organization,
		Fullname:            p.Fullname,
		Email:               p.Email,
		UID:                 p.UID,
		GID:                 p.GID,
		Shell:               p.Shell,
		Sudo:                p.Sudo,
		Source:              p.Source,
		Roles:               p.Roles,
		Blueprints:          p.Blueprints,
		AccountLocked:       p.AccountLocked,
		PasswordLocked:      p.PasswordLocked,
		PasswordLockedUntil: p.PasswordLockedUntil,
	}, nil
}

// BlueprintApps converts the blueprint's value-map of AppSpec to the pointer-map
// used internally by AppManager. It also sets the Name field from the map key.
func BlueprintApps(bpApps map[string]commonmodels.AppSpec) map[string]*commonmodels.AppSpec {
	result := make(map[string]*commonmodels.AppSpec, len(bpApps))
	for name, spec := range bpApps {
		s := spec
		s.Name = name
		result[name] = &s
	}
	return result
}
