package main

import (
	"fmt"
	"os"
	"os/exec"

	clogger "github.com/k8shell-io/common/pkg/logger"
	"github.com/k8shell-io/k8shelld/internal/config"
	"github.com/k8shell-io/k8shelld/internal/logger"
	"github.com/k8shell-io/k8shelld/internal/server"
	"github.com/k8shell-io/k8shelld/internal/system"
)

func main() {
	opts, err := getOptions(config.K8SHELLD_VERSION, config.K8SHELLD_COMMIT)
	if err != nil {
		fmt.Printf("Error parsing options: %v\n", err)
		os.Exit(1)
	}

	clogger.JsonLogger = false

	// TODO: use common logger, the k8shelld logger should adopt from common
	logger := logger.NewLogger("k8shelld")
	logger.Debug().Msgf("Options: %+v", opts)

	if os.Geteuid() != 0 {
		logger.Fatal().Msg("You must be root to run k8shelld.")
	}

	if os.Getpid() != 1 {
		logger.Fatal().Msg("k8shelld must run as PID 1.")
	}

	logger.Info().Msgf("Starting k8shelld, version: %s", config.K8SHELLD_VERSION)

	cfg, err := ValidateAndLoadConfig(opts.ConfigPath)
	if err != nil {
		logger.Fatal().Msgf("Error loading configuration: %v", err)
	}
	logger.Info().Msgf("Configuration loaded, file=%s", opts.ConfigPath)
	logger.Debug().Msgf("Configuration: %+v", cfg)

	err = exec.Command("kbox", "tools-init").Run()
	if err != nil {
		logger.Error().Msgf("Error running kbox tools-init: %v", err)
	}

	if err := system.CreateUser(cfg.User); err != nil {
		logger.Fatal().Msgf("Error creating user: %v", err)
	}

	if cfg.Docker.CreateDockerSockSymlink {
		if _, err := os.Lstat(config.DOCKER_SOCKET_SYMLINK); err != nil {
			if err := os.Symlink(config.DOCKER_SOCKET_PATH, config.DOCKER_SOCKET_SYMLINK); err != nil {
				logger.Error().Msgf("Error creating docker socket symlink: %v", err)
			} else {
				logger.Info().Msgf("Created Docker socket symlink: %s -> %s",
					config.DOCKER_SOCKET_SYMLINK, config.DOCKER_SOCKET_PATH)
			}
		} else {
			logger.Warn().Msgf("Docker socket symlink already exists: %s", config.DOCKER_SOCKET_SYMLINK)
		}
	}

	server, err := server.NewServer(cfg, opts.UnixSocketPath, opts.InitScriptsDir)
	if err != nil {
		logger.Fatal().Msgf("Error creating server: %v", err)
	}

	server.Serve()
	logger.Info().Msg("Exiting k8shelld")
}
