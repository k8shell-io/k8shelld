package main

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/k8shell-io/k8shelld/internal/log"
	"github.com/k8shell-io/k8shelld/internal/server"
)

func main() {
	opts, err := getOptions(server.K8SHELLD_VERSION, server.K8SHELLD_COMMIT)
	if err != nil {
		fmt.Printf("Error parsing options: %v\n", err)
		os.Exit(1)
	}

	if opts.test {
		//log.JsonLogger = false
	}
	logger := log.NewLogger("k8shelld")
	logger.Debug().Msgf("Options: %+v", opts)

	if !opts.test {
		if os.Geteuid() != 0 {
			logger.Fatal().Msg("You must be root to run k8shelld.")
		}
		if os.Getpid() != 1 {
			logger.Fatal().Msg("k8shelld must run as PID 1.")
		}
	}

	logger.Info().Msgf("Starting k8shelld, version: %s", server.K8SHELLD_VERSION)

	var keys = &server.Keys{A1Key: "", A2Key: ""}
	if !opts.test {
		keys, err = loadKeys()
		if err != nil {
			logger.Fatal().Msgf("error loading keys: %v", err)
		}
	} else {
		logger.Info().Msg("Test mode, not loading keys")
	}

	config, err := ValidateAndLoadConfig(opts.ConfigPath, keys.A1Key)
	if err != nil {
		logger.Fatal().Msgf("Error loading configuration: %v", err)
	}
	logger.Info().Msgf("Configuration loaded, file=%s", opts.ConfigPath)
	logger.Debug().Msgf("Configuration: %+v", config)

	if !opts.test {
		err = exec.Command("kbox", "tools-init").Run()
		if err != nil {
			logger.Error().Msgf("Error running kbox tools-init: %v", err)
		}

		if err := server.CreateUser(config.MainUser); err != nil {
			logger.Fatal().Msgf("Error creating main user: %v", err)
		}

		scripts := server.NewInitScripts(config.MainUser, opts.InitScriptsDir)
		scripts.Run()
	} else {
		logger.Info().Msg("Test mode, not initializing kbox tools and not running init scripts")
	}

	server, err := server.NewServer(config, keys, opts.ApiTCPPort, opts.ServerKeyPath,
		opts.ServerCertPath, opts.KeyLogFilePath, opts.UnixSocketPath, opts.DefaultDNS)
	if err != nil {
		logger.Fatal().Msgf("Error creating server: %v", err)
	}

	server.Serve()
	logger.Info().Msg("Exiting k8shelld")
}
