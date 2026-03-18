package main

import (
	"fmt"
	"os"

	clogger "github.com/k8shell-io/common/pkg/logger"
	"github.com/k8shell-io/k8shelld/internal/config"
	"github.com/k8shell-io/k8shelld/internal/logger"
	"github.com/k8shell-io/k8shelld/internal/server"
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

	if opts.Test {
		logger.Info().Msg("Running in test mode")
	}

	if !opts.Test {
		if os.Geteuid() != 0 {
			logger.Fatal().Msg("You must be root to run k8shelld.")
		}

		if os.Getpid() != 1 {
			logger.Fatal().Msg("k8shelld must run as PID 1.")
		}
	}

	logger.Info().Msgf("Starting k8shelld, version: %s", config.K8SHELLD_VERSION)

	cfg, err := LoadConfig(opts.ConfigPath)
	if err != nil {
		logger.Fatal().Msgf("Error loading configuration: %v", err)
	}
	logger.Info().Msgf("Configuration loaded, file=%s", opts.ConfigPath)
	logger.Debug().Msgf("Configuration: %+v", cfg)

	if opts.Port != 0 {
		cfg.System.GrpcConfig.Port = opts.Port
		logger.Info().Msgf("Overriding GRPC port to %d from command line", opts.Port)
	}

	if opts.CertFile != "" && opts.KeyFile != "" {
		logger.Info().Msgf("Overriding GRPC TLS cert and key from command line")
		cfg.System.GrpcConfig.CertFile = opts.CertFile
		cfg.System.GrpcConfig.KeyFile = opts.KeyFile
	}

	jwtVerifier, err := applyIdentityToConfig(cfg, opts.Test)
	if err != nil {
		logger.Fatal().Msgf("Error loading identity: %v", err)
	}
	if !opts.Test {
		logger.Info().Msgf("Identity token verified for user: %s (uid=%d gid=%d)",
			cfg.User.Username, cfg.User.Uid, cfg.User.Gid)
	}

	server, err := server.NewServer(cfg, opts.UnixSocketPath, opts.Test, jwtVerifier)
	if err != nil {
		logger.Fatal().Msgf("Error creating server: %v", err)
	}

	logger.Info().Msg("Starting k8shelld server...")

	server.Serve()
	logger.Info().Msg("Exiting k8shelld")
}
