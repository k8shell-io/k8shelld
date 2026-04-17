package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	config "github.com/k8shell-io/common/pkg/api/client/k8shelld"
	"github.com/k8shell-io/k8shelld/internal/logger"
	"github.com/k8shell-io/k8shelld/internal/models"
)

// Options represents the command line options
type Options struct {
	UnixSocketPath string
	ConfigPath     string
	Port           int
	CertFile       string
	KeyFile        string
	Test           bool
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
		Identity: config.Identity{
			TokenPath:     "/run/secrets/identity-token",
			PublicKeyPath: "/run/secrets/identity-public-key",
			SigningMethod: "rs256",
		},
		TerminateOrphans: config.TerminateOrphans{
			Enabled:       true,
			CheckInterval: 10,
			Exclude:       []string{},
		},
		ReapZombies: config.ReapZombies{
			Enabled: true,
		},
		InitScriptsDir: "/usr/local/k8shell/system",
	}
)

// copyConfigJSON copies the configuration struct to a new struct
func copyConfigJSON(original *config.Config) (*config.Config, error) {
	bytes, _ := json.Marshal(original)
	var newConfig config.Config
	if err := json.Unmarshal(bytes, &newConfig); err != nil {
		return nil, err
	}
	return &newConfig, nil
}

// getOptions parses the command line options and returns the Options struct
func getOptions(version string, commit_id string) (*Options, error) {
	// Default options
	options := &Options{
		UnixSocketPath: models.RESTAPIUnixSocket,
		ConfigPath:     "/etc/k8shell/config.yaml",
		showVersion:    false,
	}

	flag.StringVar(&options.ConfigPath, "config", options.ConfigPath, "Path to the configuration file")
	flag.StringVar(&options.UnixSocketPath, "socket", options.UnixSocketPath, "Unix socket path")
	flag.IntVar(&options.Port, "port", 0, "Port number for the GRPC API")
	flag.StringVar(&options.CertFile, "cert", "", "Path to the TLS certificate file")
	flag.StringVar(&options.KeyFile, "key", "", "Path to the TLS key file")
	flag.BoolVar(&options.Test, "test", false, "Enable test mode")
	flag.BoolVar(&options.showVersion, "v", false, "Show version information")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage:\n  k8shelld [options]\n")
		fmt.Fprint(os.Stderr, "\n")
		fmt.Fprintf(os.Stderr, "Options:\n")
		fmt.Fprintf(os.Stderr, "  --config <file>         Configuration file (default: %s)\n", options.ConfigPath)
		fmt.Fprintf(os.Stderr, "  --socket <file>         REST API Unix socket path (default: %s)\n", options.UnixSocketPath)
		fmt.Fprint(os.Stderr, "  --port <port>           Port number for the GRPC API\n")
		fmt.Fprint(os.Stderr, "  --cert <file>           Path to the TLS certificate file\n")
		fmt.Fprint(os.Stderr, "  --key <file>            Path to the TLS key file\n")
		fmt.Fprint(os.Stderr, "  --test                  Enable test mode\n")
		fmt.Fprint(os.Stderr, "  -v                      Show version and exit\n")
	}

	flag.Parse()
	if options.showVersion {
		fmt.Printf("k8shelld version: %s (commit: %s)\n", version, commit_id)
		os.Exit(0)
	}

	return options, nil
}

// LoadConfig loads the configuration file
func LoadConfig(configPath string) (*config.Config, error) {
	yamlData, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %v", err)
	}

	cfg, err := copyConfigJSON(defaultConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to copy default config: %v", err)
	}

	if err := yaml.Unmarshal(yamlData, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse YAML: %v", err)
	}

	err = logger.InitLogLevel(cfg.System.LogLevel)
	if err != nil {
		return nil, fmt.Errorf("failed to set log level: %v", err)
	}

	return cfg, nil
}
