package main

import (
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/invopop/jsonschema"
	"github.com/xeipuuv/gojsonschema"

	"github.com/k8shell-io/k8shelld/pkg/server"
)

// Options represents the command line options
type Options struct {
	ApiTCPPort     int
	UnixSocketPath string
	ConfigPath     string
	ServerKeyPath  string
	ServerCertPath string
	DefaultDNS     string
	InitScriptsDir string
	KeyLogFilePath string
	showVersion    bool
	test           bool
}

var (
	DEFAULT_LOG_LEVEL = "debug"
	TimeFormat        = "2006-01-02T15:04:05.000"

	// Default configuration
	defaultConfig = &server.Config{
		System: server.System{
			PProf:    false,
			LogLevel: DEFAULT_LOG_LEVEL,
		},
		DockerDNS: server.DockerDNSConf{
			Enabled:       false,
			Fqdn:          false,
			ContainerName: true,
			ContainerId:   true,
			DNSNames:      true,
			UpstreamDNS:   "",
			Searches:      []string{},
		},
		Env: server.Env{
			Unset:         []string{},
			UnsetPatterns: []*regexp.Regexp{},
		},
		PortForwarding:      []string{"localnetworks:0"},
		PortForwardingRules: []server.PortForwardingRule{},
		TerminateOrphans: server.TerminateOrphans{
			Enabled:       true,
			CheckInterval: 10,
			Exclude:       []string{},
		},
		ReapZombies: server.ReapZombies{
			Enabled: true,
		},
	}
)

// copyConfigJSON copies the configuration struct to a new struct
func copyConfigJSON(original *server.Config) *server.Config {
	bytes, _ := json.Marshal(original)
	var newConfig server.Config
	json.Unmarshal(bytes, &newConfig)
	return &newConfig
}

// getOptions parses the command line options and returns the Options struct
func getOptions(version string, commit_id string) (*Options, error) {
	// Default options
	options := &Options{
		ApiTCPPort:     2822,
		UnixSocketPath: "/var/run/k8shelld.sock",
		ConfigPath:     "/etc/k8shell/config.yaml",
		ServerKeyPath:  "/etc/k8shell/server.key",
		ServerCertPath: "/etc/k8shell/server.crt",
		DefaultDNS:     "10.96.0.10",
		InitScriptsDir: "/usr/local/k8shell/system",
		KeyLogFilePath: "",
		showVersion:    false,
	}

	// Parse command line flags
	flag.StringVar(&options.ConfigPath, "config", options.ConfigPath, "Path to the configuration file")
	flag.IntVar(&options.ApiTCPPort, "port", options.ApiTCPPort, "API TCP port")
	flag.StringVar(&options.ServerKeyPath, "server-key", options.ServerKeyPath, "Server key file")
	flag.StringVar(&options.ServerCertPath, "server-cert", options.ServerCertPath, "Server certificate file")
	flag.StringVar(&options.DefaultDNS, "default-dns", options.DefaultDNS, "Default upstream DNS")
	flag.StringVar(&options.UnixSocketPath, "socket", options.UnixSocketPath, "Unix socket path")
	flag.StringVar(&options.InitScriptsDir, "init-scripts", options.InitScriptsDir, "Directory for init scripts")
	flag.StringVar(&options.KeyLogFilePath, "keylog", options.KeyLogFilePath, "File to log TLS master secrets in NSS key log format")
	flag.BoolVar(&options.test, "test", false, "Enable test mode")
	flag.BoolVar(&options.showVersion, "v", false, "Show version information")

	// Print usage
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage:\n  k8shelld [options]\n")
		fmt.Fprint(os.Stderr, "\n")
		fmt.Fprintf(os.Stderr, "Options:\n")
		fmt.Fprintf(os.Stderr, "  --config <file>         Configuration file (default: %s)\n", options.ConfigPath)
		fmt.Fprintf(os.Stderr, "  --port <int>            GRPC API TCP port (default: %d)\n", options.ApiTCPPort)
		fmt.Fprintf(os.Stderr, "  --socket <file>         REST API Unix socket path (default: %s)\n", options.UnixSocketPath)
		fmt.Fprintf(os.Stderr, "  --default-dns <ip>   	  Default upstream DNS (default: %s)\n", options.DefaultDNS)
		fmt.Fprintf(os.Stderr, "  --server-cert <file>    Server certificate file (default: %s)\n", options.ServerCertPath)
		fmt.Fprintf(os.Stderr, "  --server-key <file>     Server key file (default: %s)\n", options.ServerKeyPath)
		fmt.Fprintf(os.Stderr, "  --init-scripts <dir>    Directory for init scripts (default: %s)\n", options.InitScriptsDir)
		fmt.Fprint(os.Stderr, "  --keylog <file>         File to log TLS master secrets in NSS key log format\n", options.KeyLogFilePath)
		fmt.Fprint(os.Stderr, "  --test                  Enable test mode\n")
		fmt.Fprint(os.Stderr, "  -v                      Show version and exit\n")
	}

	// Parse the flags
	flag.Parse()
	if options.showVersion {
		fmt.Printf("k8shelld version: %s (commit: %s)\n", version, commit_id)
		os.Exit(0)
	}

	return options, nil
}

// LoadKeys reads the access keys from environment variables and unsets them after reading
// This is done to prevent the keys from being leaked in the process environment.
// There are two keys: A1K for the GRPC API and A2K for k8shell REST API
func loadKeys() (*server.Keys, error) {
	keys := &server.Keys{}

	keys.A1Key = os.Getenv("A1K")
	if keys.A1Key == "" {
		return nil, fmt.Errorf("A1K environment variable is not set")
	}
	os.Unsetenv("A1K")

	keys.A2Key = os.Getenv("A2K")
	if keys.A2Key == "" {
		return nil, fmt.Errorf("A2K environment variable is not set")
	}
	os.Unsetenv("A2K")

	return keys, nil
}

// ValidateAndLoadConfig validates and loads the configuration file
func ValidateAndLoadConfig(configPath string, accessKey string) (*server.Config, error) {
	yamlData, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %v", err)
	}

	// config is base64 encoded and can be encrypted
	if strings.HasPrefix(string(yamlData), "ENC[AES256]") {
		decryptedData, err := server.DecryptAES(accessKey, yamlData)
		if err != nil {
			return nil, fmt.Errorf("failed to decrypt config file: %v", err)
		}
		yamlData = decryptedData
	} else {
		decoded, err := base64.StdEncoding.DecodeString(string(yamlData))
		if err == nil {
			yamlData = decoded
		} else {
			yamlData = []byte(string(yamlData))
		}
	}

	config := copyConfigJSON(defaultConfig)
	if err := yaml.Unmarshal(yamlData, &config); err != nil {
		return nil, fmt.Errorf("failed to parse YAML: %v", err)
	}

	jsonData, err := json.Marshal(config)
	if err != nil {
		return nil, fmt.Errorf("failed to convert YAML to JSON: %v", err)
	}

	schema := jsonschema.Reflect(&server.Config{})
	schemaJSON, err := json.Marshal(schema)
	if err != nil {
		return nil, fmt.Errorf("failed to generate schema: %v", err)
	}

	loader := gojsonschema.NewStringLoader(string(jsonData))
	schemaLoader := gojsonschema.NewStringLoader(string(schemaJSON))
	result, err := gojsonschema.Validate(schemaLoader, loader)
	if err != nil {
		return nil, fmt.Errorf("failed to validate schema: %v", err)
	}

	if !result.Valid() {
		for _, err := range result.Errors() {
			fmt.Printf("Validation error: %s\n", err)
		}
		return nil, fmt.Errorf("config file does not match schema")
	}

	// Set the log level
	err = server.SetLogLevel(config.System.LogLevel)
	if err != nil {
		return nil, fmt.Errorf("failed to set log level: %v", err)
	}

	// Set the home directory for the main user
	config.MainUser.HomeDir = fmt.Sprintf("/home/%s", config.MainUser.Username)

	// Parse port forwarding allow rules
	for _, rule := range config.PortForwarding {
		parsedRule, err := server.ParsePortForwardingRule(rule)
		if err != nil {
			return nil, fmt.Errorf("failed to parse port forwarding rule: %v", err)
		}
		config.PortForwardingRules = append(config.PortForwardingRules, parsedRule)
	}

	// Parse env unset patterns and compile them
	for _, pattern := range config.Env.Unset {
		p, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("failed to compile env unset pattern: %v", err)
		}
		config.Env.UnsetPatterns = append(config.Env.UnsetPatterns, p)
	}

	return config, nil
}
