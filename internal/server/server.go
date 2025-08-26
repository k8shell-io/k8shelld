package server

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/k8shell-io/k8shelld/internal/config"
	"github.com/k8shell-io/k8shelld/internal/grpc"
	"github.com/k8shell-io/k8shelld/internal/log"
	"github.com/k8shell-io/k8shelld/internal/system"
	"github.com/rs/zerolog"
)

type Server struct {
	logger      *zerolog.Logger
	config      *config.Config
	restService *RESTService
	grpcService *grpc.GRPCService
	procWatcher *system.ProcessWatcher
	pprof       bool
	sysInfo     *system.SystemInfo
	sysInfoMu   sync.Mutex
}

func NewServer(cfg *config.Config, keys *config.Keys, grpcApiListenPort int, serverKeyPath string, serverCertPath string,
	keyLogFilePath string, restApiUnixSocketPath string, initScriptsDir string) (*Server, error) {

	s := &Server{
		logger:  log.NewLogger("k8shelld"),
		config:  cfg,
		pprof:   cfg.System.PProf,
		sysInfo: nil,
	}

	var err error
	s.procWatcher = system.NewProcessWatcher(cfg.TerminateOrphans.Enabled, cfg.ReapZombies.Enabled,
		cfg.TerminateOrphans.CheckInterval, cfg.TerminateOrphans.Exclude)

	s.grpcService, err = grpc.NewGRPCService(grpcApiListenPort, keys.A1Key, cfg.User, serverKeyPath,
		serverCertPath, keyLogFilePath, cfg.PortForwardingRules, initScriptsDir, s.procWatcher)
	if err != nil {
		return nil, fmt.Errorf("error creating GRPC API: %v", err)
	}

	s.restService, err = NewRESTService(restApiUnixSocketPath, cfg.User, s)
	if err != nil {
		return nil, fmt.Errorf("error creating REST API: %v", err)
	}

	config.UnsetEnvVars(cfg.Env)
	return s, nil
}

func (s *Server) Serve() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup

	// gRPC handler
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.grpcService.Serve(ctx)
	}()

	// REST handler
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.restService.Serve(ctx)
	}()

	// process watcher handler
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.procWatcher.Run(ctx)
	}()

	// system info handler
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				s.sysInfoMu.Lock()
				newInfo, err := system.UpdateSystemInfo(s.sysInfo)
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

	// pprof if enabled
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
