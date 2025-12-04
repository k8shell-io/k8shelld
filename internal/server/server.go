package server

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/k8shell-io/api-server/pkg/client"
	"github.com/k8shell-io/k8shelld/internal/config"
	"github.com/k8shell-io/k8shelld/internal/grpc"
	"github.com/k8shell-io/k8shelld/internal/logger"
	"github.com/k8shell-io/k8shelld/internal/system"
	"github.com/rs/zerolog"
)

type Server struct {
	logger      *zerolog.Logger
	testMode    bool
	config      *config.Config
	workspace   string
	restService *RESTService
	grpcService *grpc.GRPCService
	procWatcher *system.ProcessWatcher
	apiClient   *client.Client
	pprof       bool
	sysInfo     *system.SystemInfo
	sysInfoMu   sync.Mutex
	appManager  *system.AppManager
}

func NewServer(cfg *config.Config, restApiUnixSocketPath string, testMode bool) (*Server, error) {

	s := &Server{
		logger:    logger.NewLogger("k8shelld"),
		testMode:  testMode,
		config:    cfg,
		pprof:     cfg.System.PProf,
		sysInfo:   nil,
		apiClient: client.NewClient(cfg.System.ApiServer, cfg.User.UserToken),
	}

	var err error
	s.workspace = os.Getenv("WORKSPACE")
	if s.workspace == "" {
		return nil, fmt.Errorf("cannot get the workspace name from WORKSPACE environment variable")
	}

	if !s.testMode {
		s.procWatcher = system.NewProcessWatcher(cfg.TerminateOrphans.Enabled, cfg.ReapZombies.Enabled,
			cfg.TerminateOrphans.CheckInterval, cfg.TerminateOrphans.Exclude)
	} else {
		s.procWatcher = system.NewProcessWatcher(false, false, 0, nil)
	}

	s.grpcService, err = grpc.NewGRPCService(cfg.User, cfg.System.GrpcConfig, cfg.PortForwardingRules,
		cfg.InitScriptsDir, s.procWatcher, s.apiClient)
	if err != nil {
		return nil, fmt.Errorf("error creating GRPC API: %v", err)
	}

	s.restService, err = NewRESTService(restApiUnixSocketPath, cfg.User, s)
	if err != nil {
		return nil, fmt.Errorf("error creating REST API: %v", err)
	}

	s.appManager, err = system.NewAppManager(cfg.Apps, cfg.User, s.procWatcher, system.APPS_DIR, s.testMode)
	if err != nil {
		return nil, fmt.Errorf("error creating App Manager: %v", err)
	}

	config.UnsetEnvVars(cfg.Env)

	err = s.initialize()
	if err != nil {
		return nil, fmt.Errorf("error initializing server: %v", err)
	}

	return s, nil
}

func (s *Server) initialize() error {
	if s.testMode {
		s.logger.Info().Msg("Test mode enabled, skipping initialization")
		return nil
	}

	err := exec.Command("kbox", "tools-init").Run()
	if err != nil {
		s.logger.Error().Msgf("Error running kbox tools-init: %v", err)
	}

	if err := system.CreateUser(s.config.User); err != nil {
		s.logger.Fatal().Msgf("Error creating user: %v", err)
	}

	if s.config.Docker.CreateDockerSockSymlink {
		if _, err := os.Lstat(config.DOCKER_SOCKET_SYMLINK); err != nil {
			if err := os.Symlink(config.DOCKER_SOCKET_PATH, config.DOCKER_SOCKET_SYMLINK); err != nil {
				s.logger.Error().Msgf("Error creating docker socket symlink: %v", err)
			} else {
				s.logger.Info().Msgf("Created Docker socket symlink: %s -> %s",
					config.DOCKER_SOCKET_SYMLINK, config.DOCKER_SOCKET_PATH)
			}
		} else {
			s.logger.Warn().Msgf("Docker socket symlink already exists: %s", config.DOCKER_SOCKET_SYMLINK)
		}
	}
	return nil
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

	if !s.testMode {
		wg.Add(1)
		go func() {
			defer wg.Done()
			system.KillAllProcesses(s.logger)
		}()
	}

	cancel()
	wg.Wait()

	s.logger.Info().Msgf("Shutdown complete.")
}
