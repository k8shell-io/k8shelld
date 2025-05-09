package server

import (
	"fmt"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
)

var (
	K8SHELLD_VERSION = "0.0.0"
	K8SHELLD_COMMIT  = "0000000"
)

// Maximum packet size for streaming data
const (
	DEFAULT_MAX_PACKET_SIZE = 1 << 15 // 32 KB
)

// Configuration file structure
// The configuration file is a YAML file that contains the following fields:
// - system: General system configuration.
// - mainUser: The main user that will be created when the service starts.
// - extraUsers: A list of additional users that will be created when the service starts.
// - dockerDNS: Configuration for the Docker DNS feature.
// - portForwarding: Configuration for the port forwarding feature.
// - terminateOrphans: Configuration for the terminate orphans feature.
// - reapZombies: Configuration for the reap zombies feature.
// - logger: Configuration for the logger.

// System represents the general system configuration
type System struct {
	PProf    bool   `yaml:"pprof"`
	LogLevel string `yaml:"logLevel" jsonschema:"enum=debug,enum=info,enum=warn,enum=error,default=info"`
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

// Keys represents the keys
type Keys struct {
	A1Key string // Access key for GRPC API
	A2Key string // Access key for REST API
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

// Config represents the main configuration file structure
type Config struct {
	System              System           `yaml:"system"`
	MainUser            MainUser         `yaml:"mainUser"`
	Env                 Env              `yaml:"env"`
	DockerDNS           DockerDNSConf    `yaml:"dockerDNS"`
	PortForwarding      []string         `yaml:"portForwarding"`
	TerminateOrphans    TerminateOrphans `yaml:"terminateOrphans"`
	ReapZombies         ReapZombies      `yaml:"reapZombies"`
	PortForwardingRules []PortForwardingRule
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

	subnet := &net.IPNet{}
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

	return PortForwardingRule{Subnet: subnet, Port: uint16(port)}, nil
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
