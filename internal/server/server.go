package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/k8shell-io/k8shelld/internal/log"
	"github.com/rs/zerolog"
)

type Server struct {
	logger    *zerolog.Logger
	restApi   *RESTApiService
	grpcApi   *GRPCApiService
	dns       *DockerDNS
	proc      *ProcessWatcher
	pprof     bool
	sysInfo   *SystemInfo
	sysInfoMu sync.Mutex
}

func NewServer(config *Config, keys *Keys, grpcApiListenPort int, serverKeyPath string, serverCertPath string,
	keyLogFilePath string, restpApiUnixSocket string, defaultDNS string) (*Server, error) {
	server := &Server{logger: log.NewLogger("k8shelld"), pprof: config.System.PProf, sysInfo: nil}
	var err error

	// Create GRPC API service
	server.grpcApi, err = NewGRPCAPI(grpcApiListenPort, keys.A1Key, config.MainUser, serverKeyPath,
		serverCertPath, keyLogFilePath, config.PortForwardingRules)
	if err != nil {
		return nil, fmt.Errorf("error creating GRPC API: %v", err)
	}

	// Create Docker DNS
	if config.DockerDNS.Enabled {
		server.dns, err = NewDockerDNS(config.DockerDNS.Fqdn, config.DockerDNS.ContainerName,
			config.DockerDNS.ContainerId, config.DockerDNS.DNSNames, config.DockerDNS.UpstreamDNS,
			config.DockerDNS.Searches, defaultDNS)
		if err != nil {
			return nil, fmt.Errorf("error creating Docker DNS instance: %v", err)
		} else {
			// Start Docker DNS
			server.dns.Run()
			defer server.dns.Stop()
		}
	}

	// Create API service
	server.restApi, err = NewRESTAPI(keys.A2Key, restpApiUnixSocket, config.MainUser, server)
	errors.Is(err, context.Canceled)
	if err != nil {
		return nil, fmt.Errorf("error creating REST API: %v", err)
	}

	// Create process watcher
	server.proc = NewProcessWatcher(config.TerminateOrphans.Enabled, config.ReapZombies.Enabled,
		config.TerminateOrphans.CheckInterval, config.TerminateOrphans.Exclude)

	// Unset environment variables
	UnsetEnvVars(config.Env)

	return server, nil
}

func (s *Server) Serve() {
	// Context will be canceled on SIGTERM or SIGINT
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup

	// Start gRPC handler
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.grpcApi.Handler(ctx)
	}()

	// Start REST handler
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.restApi.Handler(ctx)
	}()

	// Start process handler
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.proc.Handler(ctx)
	}()

	// Start system info handler
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				s.sysInfoMu.Lock()
				newInfo, err := UpdateSystemInfo(s.sysInfo)
				if err != nil {
					s.logger.Warn().Msgf("Failed to update system info: %v", err)
					s.sysInfoMu.Unlock()
					continue
				}
				s.sysInfo = newInfo
				s.sysInfoMu.Unlock()
			case <-ctx.Done():
				s.logger.Info().Msg("System info updater stopped.")
				return
			}
		}
	}()

	// Start pprof if enabled
	if s.pprof {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.logger.Info().Msg("Starting pprof on :6060")
			if err := http.ListenAndServe("localhost:6060", nil); err != nil && err != http.ErrServerClosed {
				s.logger.Error().Msgf("pprof error: %v", err)
			}
			s.logger.Info().Msg("pprof stopped")
		}()
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGTERM, syscall.SIGINT)

	sig := <-sigChan
	s.logger.Info().Msgf("Received signal: %s. Initiating shutdown...", sig)
	cancel()
	wg.Wait()

	s.logger.Info().Msgf("Shutdown complete.")
}
