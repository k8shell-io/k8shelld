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
)

type Server struct {
	logger  *Logger
	restApi *RESTApiService
	grpcApi *GRPCApiService
	scripts *InitScripts
	dns     *DockerDNS
	proc    *ProcessWatcher
	pprof   bool
}

func NewServer(config *Config, keys *Keys, grpcApiListenPort int, serverKeyPath string, serverCertPath string,
	keyLogFilePath string, restpApiUnixSocket string, defaultDNS string, initScriptsDir string) (*Server, error) {
	server := &Server{logger: NewLogger("k8shelld"), pprof: config.System.PProf}
	var err error

	// Default to real execution
	runner := &RealCommandRunner{}

	// Default to real filesystem
	filesystem := &RealFileSystem{}

	// Default to real user lookup
	userlookup := &RealUserLookup{}

	// Create context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Create main user
	if err := CreateMainUser(ctx, runner, filesystem, userlookup, config.MainUser); err != nil {
		return nil, fmt.Errorf("error creating main user: %v", err)
	}

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
	server.restApi, err = NewRESTAPI(keys.A2Key, restpApiUnixSocket, config.MainUser, server.grpcApi, server.dns)
	if err != nil {
		return nil, fmt.Errorf("error creating REST API: %v", err)
	}

	// Initialize the init scripts
	server.scripts = NewInitScripts(config.MainUser, initScriptsDir)

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

	// Run workspace init scripts
	s.scripts.Run()

	// Create a wait group to wait for all goroutines to finish
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

	// Start pprof if enabled
	if s.pprof {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.logger.Info("Starting pprof on :6060")
			if err := http.ListenAndServe("localhost:6060", nil); err != nil && err != http.ErrServerClosed {
				s.logger.Error("pprof error: %v", err)
			}
			s.logger.Info("pprof stopped")
		}()
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGTERM, syscall.SIGINT)

	sig := <-sigChan
	s.logger.Info("Received signal: %s. Initiating shutdown...", sig)
	cancel()
	wg.Wait()

	s.logger.Info("Shutdown complete.")
}
