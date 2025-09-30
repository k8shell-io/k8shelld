package main

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/k8shell-io/k8shelld/internal/config"
	"github.com/k8shell-io/k8shelld/internal/log"
	"github.com/k8shell-io/k8shelld/internal/server"
	"github.com/k8shell-io/k8shelld/internal/system"
)

func main() {
	opts, err := getOptions(config.K8SHELLD_VERSION, config.K8SHELLD_COMMIT)
	if err != nil {
		fmt.Printf("Error parsing options: %v\n", err)
		os.Exit(1)
	}

	logger := log.NewLogger("k8shelld")
	logger.Debug().Msgf("Options: %+v", opts)

	if os.Geteuid() != 0 {
		logger.Fatal().Msg("You must be root to run k8shelld.")
	}

	if os.Getpid() != 1 {
		logger.Fatal().Msg("k8shelld must run as PID 1.")
	}

	logger.Info().Msgf("Starting k8shelld, version: %s", config.K8SHELLD_VERSION)

	keys, err := loadKeys()
	if err != nil {
		logger.Fatal().Msgf("error loading keys: %v", err)
	}

	config, err := ValidateAndLoadConfig(opts.ConfigPath, keys.A1Key)
	if err != nil {
		logger.Fatal().Msgf("Error loading configuration: %v", err)
	}
	logger.Info().Msgf("Configuration loaded, file=%s", opts.ConfigPath)
	logger.Debug().Msgf("Configuration: %+v", config)

	err = exec.Command("kbox", "tools-init").Run()
	if err != nil {
		logger.Error().Msgf("Error running kbox tools-init: %v", err)
	}

	if err := system.CreateUser(config.User); err != nil {
		logger.Fatal().Msgf("Error creating user: %v", err)
	}

	dockerSocketPath := "/var/run/docker/docker.sock"
	dockerSocketSymlink := "/var/run/docker.sock"
	if _, err := os.Lstat(dockerSocketSymlink); err != nil {
		if err := os.Symlink(dockerSocketPath, dockerSocketSymlink); err != nil {
			logger.Error().Msgf("Error creating docker socket symlink: %v", err)
		} else {
			logger.Info().Msgf("Created Docker socket symlink: %s -> %s", dockerSocketSymlink, dockerSocketPath)
		}
	}

	server, err := server.NewServer(config, keys, opts.ApiTCPPort, opts.ServerKeyPath,
		opts.ServerCertPath, opts.KeyLogFilePath, opts.UnixSocketPath, opts.InitScriptsDir)
	if err != nil {
		logger.Fatal().Msgf("Error creating server: %v", err)
	}

	server.Serve()
	logger.Info().Msg("Exiting k8shelld")
}
