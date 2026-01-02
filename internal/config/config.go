package config

import (
	"fmt"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/k8shell-io/common/pkg/gapi"
	"github.com/k8shell-io/k8shelld/internal/types"
)

// safeIntToUint16 safely converts int to uint16, clamping to 0-65535.
func safeIntToUint16(v int) uint16 {
	if v < 0 {
		return 0
	}
	if v > 65535 {
		return 65535
	}
	return uint16(v)
}

// Maximum packet size for streaming data
const (
	DEFAULT_MAX_PACKET_SIZE = 1 << 15 // 32 KB
)

var (
	K8SHELLD_VERSION = "0.0.0"
	K8SHELLD_COMMIT  = "0000000"
)

const (
	DOCKER_SOCKET_PATH    = "/var/run/docker/docker.sock"
	DOCKER_SOCKET_SYMLINK = "/var/run/docker.sock"
)

// Config represents the main configuration file structure
type Config struct {
	System              System           `yaml:"system"`
	User                types.User       `yaml:"user"`
	Env                 Env              `yaml:"env"`
	PortForwarding      []string         `yaml:"portForwarding"`
	TerminateOrphans    TerminateOrphans `yaml:"terminateOrphans"`
	ReapZombies         ReapZombies      `yaml:"reapZombies"`
	Docker              DockerConfig     `yaml:"docker"`
	PortForwardingRules []PortForwardingRule
	InitScriptsDir      string `yaml:"initScriptsDir"`
	EnableApps          bool   `yaml:"enableApps"`
	Apps                *Apps  `yaml:"apps" json:"apps"`
}

// System represents the general system configuration
type System struct {
	PProf      bool              `yaml:"pprof"`
	LogLevel   string            `yaml:"logLevel" jsonschema:"enum=debug,enum=info,enum=warn,enum=error,default=info"`
	ApiServer  ApiServerConfig   `yaml:"apiServer"`
	GrpcConfig gapi.ServerConfig `yaml:"grpc"`
}

type ApiServerConfig struct {
	Enabled bool   `yaml:"enabled"`
	Address string `yaml:"address"`
}

// TerminateOrphans represents the configuration for the terminate orphans feature of process watcher
type TerminateOrphans struct {
	Enabled       bool     `yaml:"enabled"`
	CheckInterval int      `yaml:"checkInterval"`
	Exclude       []string `yaml:"exclude"`
}

// ReapZombies represents the configuration for the reap zombies feature of process watcher
type ReapZombies struct {
	Enabled bool `yaml:"enabled"`
}

// Env represents the environment variables (regexp patterns) to be unset in the workspace
type Env struct {
	Unset         []string `yaml:"unset"`
	UnsetPatterns []*regexp.Regexp
}

// Rule represents a parsed rule (CIDR and Port)
type PortForwardingRule struct {
	Subnet *net.IPNet
	Port   uint16
}

// DockerConfig represents the configuration for the Docker feature
type DockerConfig struct {
	CreateDockerSockSymlink bool `yaml:"createDockerSockSymlink"`
}

// Apps represents a map of application specifications
type Apps map[string]*AppSpec

// AppSpec represents the specification for an application
type AppSpec struct {
	Name              string        `yaml:"name"`
	Binary            string        `yaml:"binary"`
	VersionCmd        []string      `yaml:"versionCmd,omitempty"`
	VersionRegex      string        `yaml:"versionRegex,omitempty"`
	Install           string        `yaml:"install"`
	Start             []string      `yaml:"start"`
	Listen            int           `yaml:"listen"`
	RestartPolicy     string        `yaml:"restartPolicy"`
	MaxRestartBackoff time.Duration `yaml:"maxRestartBackoff"`
	InstallAsRoot     bool          `yaml:"installAsRoot"`
	AutoStart         bool          `yaml:"autoStart"`
	Protocol          string        `yaml:"protocol"`
}

// parsePortForwardingRule converts a rule into a PortForwardingRule struct
// The rule format is: localnetworks[:<port>], localhost or <cidr>[:<port>]
// When the port is not specified, it defaults to 0 (all ports)
func ParsePortForwardingRule(rule string) (PortForwardingRule, error) {
	parts := strings.Split(rule, ":")
	cidr := parts[0]
	port := 0 // Default to all ports

	if len(parts) == 2 {
		var err error
		port, err = strconv.Atoi(parts[1])
		if err != nil {
			return PortForwardingRule{}, fmt.Errorf("invalid port in rule: %s", rule)
		}
	} else if len(parts) > 2 {
		return PortForwardingRule{}, fmt.Errorf("invalid rule format: %s", rule)
	}

	var subnet *net.IPNet
	var err error
	if cidr != "localnetworks" {
		if cidr == "localhost" {
			cidr = "127.0.0.1"
		}
		if cidr == "0.0.0.0" {
			cidr = "0.0.0.0/0"
		}
		if !strings.Contains(cidr, "/") {
			cidr = cidr + "/32"
		}
		_, subnet, err = net.ParseCIDR(cidr)
		if err != nil {
			return PortForwardingRule{}, fmt.Errorf("invalid CIDR in rule: %s", rule)
		}
	} else {
		subnet = nil // Special case for local networks
	}

	return PortForwardingRule{Subnet: subnet, Port: safeIntToUint16(port)}, nil
}

// UnsetEnvVars unsets the environment variables that match the patterns
func UnsetEnvVars(env Env) {
	for _, pattern := range env.UnsetPatterns {
		for _, e := range os.Environ() {
			pair := strings.SplitN(e, "=", 2)
			if pattern.MatchString(pair[0]) {
				os.Unsetenv(pair[0])
			}
		}
	}
}
