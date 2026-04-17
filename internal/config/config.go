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
