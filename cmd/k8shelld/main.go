package main

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/k8shell-io/k8shelld/pkg/server"
)

func main() {
	opts, err := getOptions(server.K8SHELLD_VERSION, server.K8SHELLD_COMMIT)
	if err != nil {
		fmt.Printf("Error parsing options: %v\n", err)
		os.Exit(1)
	}

	if opts.test {
		server.UseJSONFormatter = false
	}
	logger := server.NewLogger("k8shelld")
	logger.Debug("Options: %+v", opts)

	if !opts.test {
		if os.Geteuid() != 0 {
			logger.Fatal("You must be root to run k8shelld.")
		}
		if os.Getpid() != 1 {
			logger.Fatal("k8shelld must run as PID 1.")
		}
	}

	logger.Info("Starting k8shelld, version: %s", server.K8SHELLD_VERSION)

	var keys = &server.Keys{A1Key: "", A2Key: ""}
	if !opts.test {
		keys, err = loadKeys()
		if err != nil {
			logger.Fatal("error loading keys: %v", err)
		}
	} else {
		logger.Info("Test mode, not loading keys")
	}

	config, err := ValidateAndLoadConfig(opts.ConfigPath, keys.A1Key)
	if err != nil {
		logger.Fatal("Error loading configuration: %v", err)
	}
	logger.Info("Configuration loaded, file=%s", opts.ConfigPath)
	logger.Debug("Configuration: %+v", config)

	if !opts.test {
		err = exec.Command("kbox", "tools-init").Run()
		if err != nil {
			logger.Error("Error running kbox tools-init: %v", err)
		}

		if err := server.CreateUser(config.MainUser); err != nil {
			logger.Fatal("Error creating main user: %v", err)
		}

		scripts := server.NewInitScripts(config.MainUser, opts.InitScriptsDir)
		scripts.Run()
	} else {
		logger.Info("Test mode, not initializing kbox tools and not running init scripts")
	}

	server, err := server.NewServer(config, keys, opts.ApiTCPPort, opts.ServerKeyPath,
		opts.ServerCertPath, opts.KeyLogFilePath, opts.UnixSocketPath, opts.DefaultDNS)
	if err != nil {
		logger.Fatal("Error creating server: %v", err)
	}

	server.Serve()
	logger.Info("Exiting k8shelld")
}
