package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"regexp"

	"gopkg.in/yaml.v3"

	"github.com/invopop/jsonschema"
	"github.com/xeipuuv/gojsonschema"

	"github.com/k8shell-io/k8shelld/internal/config"
	"github.com/k8shell-io/k8shelld/internal/log"
	"github.com/k8shell-io/k8shelld/internal/models"
)

// Options represents the command line options
type Options struct {
	UnixSocketPath string
	ConfigPath     string
	InitScriptsDir string
	showVersion    bool
}

var (
	DEFAULT_LOG_LEVEL = "debug"
	TimeFormat        = "2006-01-02T15:04:05.000"

	// Default configuration
	defaultConfig = &config.Config{
		System: config.System{
			PProf:    false,
			LogLevel: DEFAULT_LOG_LEVEL,
		},
		Env: config.Env{
			Unset:         []string{},
			UnsetPatterns: []*regexp.Regexp{},
		},
		PortForwarding:      []string{"localnetworks:0"},
		PortForwardingRules: []config.PortForwardingRule{},
		TerminateOrphans: config.TerminateOrphans{
			Enabled:       true,
			CheckInterval: 10,
			Exclude:       []string{},
		},
		ReapZombies: config.ReapZombies{
			Enabled: true,
		},
	}
)

// copyConfigJSON copies the configuration struct to a new struct
func copyConfigJSON(original *config.Config) *config.Config {
	bytes, _ := json.Marshal(original)
	var newConfig config.Config
	json.Unmarshal(bytes, &newConfig)
	return &newConfig
}

// getOptions parses the command line options and returns the Options struct
func getOptions(version string, commit_id string) (*Options, error) {
	// Default options
	options := &Options{
		UnixSocketPath: models.RESTAPIUnixSocket,
		ConfigPath:     "/etc/k8shell/config.yaml",
		InitScriptsDir: "/usr/local/k8shell/system",
		showVersion:    false,
	}

	// Parse command line flags
	flag.StringVar(&options.ConfigPath, "config", options.ConfigPath, "Path to the configuration file")
	flag.StringVar(&options.UnixSocketPath, "socket", options.UnixSocketPath, "Unix socket path")
	flag.StringVar(&options.InitScriptsDir, "init-scripts", options.InitScriptsDir, "Directory for init scripts")
	flag.BoolVar(&options.showVersion, "v", false, "Show version information")

	// Print usage
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage:\n  k8shelld [options]\n")
		fmt.Fprint(os.Stderr, "\n")
		fmt.Fprintf(os.Stderr, "Options:\n")
		fmt.Fprintf(os.Stderr, "  --config <file>         Configuration file (default: %s)\n", options.ConfigPath)
		fmt.Fprintf(os.Stderr, "  --socket <file>         REST API Unix socket path (default: %s)\n", options.UnixSocketPath)
		fmt.Fprintf(os.Stderr, "  --init-scripts <dir>    Directory for init scripts (default: %s)\n", options.InitScriptsDir)
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

// ValidateAndLoadConfig validates and loads the configuration file
func ValidateAndLoadConfig(configPath string) (*config.Config, error) {
	yamlData, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %v", err)
	}

	cfg := copyConfigJSON(defaultConfig)
	if err := yaml.Unmarshal(yamlData, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse YAML: %v", err)
	}

	jsonData, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to convert YAML to JSON: %v", err)
	}

	schema := jsonschema.Reflect(&config.Config{})
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
	err = log.InitLogLevel(cfg.System.LogLevel)
	if err != nil {
		return nil, fmt.Errorf("failed to set log level: %v", err)
	}

	// Set the home directory for the workspace user
	cfg.User.HomeDir = fmt.Sprintf("/home/%s", cfg.User.Username)

	// Parse port forwarding allow rules
	for _, rule := range cfg.PortForwarding {
		parsedRule, err := config.ParsePortForwardingRule(rule)
		if err != nil {
			return nil, fmt.Errorf("failed to parse port forwarding rule: %v", err)
		}
		cfg.PortForwardingRules = append(cfg.PortForwardingRules, parsedRule)
	}

	// Parse env unset patterns and compile them
	for _, pattern := range cfg.Env.Unset {
		p, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("failed to compile env unset pattern: %v", err)
		}
		cfg.Env.UnsetPatterns = append(cfg.Env.UnsetPatterns, p)
	}

	return cfg, nil
}
