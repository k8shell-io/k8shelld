package main

import (
	"os"
	"os/exec"

	"github.com/k8shell-io/k8shelld/pkg/server"
)

func main() {
	// Create main logger
	logger := server.NewLogger("k8shelld")

	// Parse command line options
	opts, err := getOptions(server.K8SHELLD_VERSION, server.K8SHELLD_COMMIT)
	if err != nil {
		logger.Fatal("Error getting k8shelld options: %v", err)
	}

	// Check if the process is running as the root user
	if os.Geteuid() != 0 {
		logger.Fatal("You must be root to run k8shelld.")
	}

	// k8shelld must run as PID 1
	if os.Getpid() != 1 {
		logger.Fatal("k8shelld must run as PID 1.")
	}

	// Print version
	logger.Info("Starting k8shelld, version: %s", server.K8SHELLD_VERSION)

	// Load keys
	keys, err := loadKeys()
	if err != nil {
		logger.Fatal("error loading keys: %v", err)
	}

	// Load configuration
	config, err := ValidateAndLoadConfig(opts.ConfigPath, keys.A1Key)
	if err != nil {
		logger.Fatal("Error loading configuration: %v", err)
	}
	logger.Info("Configuration loaded, file=%s", opts.ConfigPath)
	logger.Debug("Configuration: %+v", config)

	// Initialize kbox tools
	err = exec.Command("kbox", "tools-init").Run()
	if err != nil {
		logger.Error("Error running kbox tools-init: %v", err)
	}

	server, err := server.NewServer(config, keys, opts.ApiTCPPort, opts.UnixSocketPath, opts.ServerKeyPath, opts.ServerCertPath,
		opts.KeyLogFilePath, opts.DefaultDNS, opts.InitScriptsDir)
	if err != nil {
		logger.Fatal("Error creating server: %v", err)
	}

	server.Serve()
	logger.Info("Exiting k8shelld")

}
