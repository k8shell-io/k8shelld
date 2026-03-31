package server

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/k8shell-io/api-server/pkg/client"
	"github.com/k8shell-io/common/pkg/authz"
	"github.com/k8shell-io/k8shelld/internal/apps"
	"github.com/k8shell-io/k8shelld/internal/config"
	"github.com/k8shell-io/k8shelld/internal/grpc"
	"github.com/k8shell-io/k8shelld/internal/logger"
	"github.com/k8shell-io/k8shelld/internal/models"
	"github.com/k8shell-io/k8shelld/internal/system"
	"github.com/rs/zerolog"
)

const (
	FLAG_DIR_TEMPLATE = "%s/.k8shell/flags"
)

type Server struct {
	logger      *zerolog.Logger
	testMode    bool
	user        *models.User
	config      *config.Config
	workspace   string
	restService *RESTService
	grpcService *grpc.GRPCService
	procWatcher *system.ProcessWatcher
	apiClientx  *client.Client
	pprof       bool
	sysInfo     *system.SystemInfo
	appManager  *apps.AppManager
	jwtVerifier *authz.JWTVerifier
}

func NewServer(cfg *config.Config, restApiUnixSocketPath string, testMode bool) (*Server, error) {

	var apiClient *client.Client
	if cfg.System.ApiServer.Enabled {
		if cfg.System.ApiServer.Address == "" {
			return nil, fmt.Errorf("api server is enabled but address is empty")
		}
		apiClient = client.NewClient(cfg.System.ApiServer.Address, "")
	}

	var jwtVerifier *authz.JWTVerifier
	var err error
	if !testMode {
		if cfg.Identity.TokenPath == "" || cfg.Identity.PublicKeyPath == "" || cfg.Identity.SigningMethod == "" {
			return nil, fmt.Errorf("identity config is incomplete: tokenPath, publicKeyPath and signingMethod are all required")
		}
		jwtVerifier, err = authz.NewJWTVerifier(authz.JWTVerifierConfig{
			SigningMethod: cfg.Identity.SigningMethod,
			PublicKeyFile: cfg.Identity.PublicKeyPath,
		})
		if err != nil {
			return nil, fmt.Errorf("create JWT verifier: %w", err)
		}
	}

	s := &Server{
		logger:      logger.NewLogger("k8shelld"),
		testMode:    testMode,
		config:      cfg,
		pprof:       cfg.System.PProf,
		sysInfo:     system.NewSystemInfo(cfg),
		apiClientx:  apiClient,
		jwtVerifier: jwtVerifier,
	}

	err = s.loadIdentity()
	if err != nil {
		return nil, fmt.Errorf("error loading identity: %v", err)
	}

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

	if cfg.EnableApps {
		s.appManager, err = apps.NewAppManager(cfg.Apps, s.user, s.procWatcher, s.testMode)
		if err != nil {
			return nil, fmt.Errorf("error creating App Manager: %v", err)
		}
	}

	s.grpcService, err = grpc.NewGRPCService(cfg, s.user, s.jwtVerifier, cfg.PortForwardingRules,
		s.procWatcher, s.apiClientx, s.appManager, s.sysInfo)
	if err != nil {
		return nil, fmt.Errorf("error creating GRPC API: %v", err)
	}

	s.restService, err = NewRESTService(restApiUnixSocketPath, s.user, s)
	if err != nil {
		return nil, fmt.Errorf("error creating REST API: %v", err)
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

	if err := system.CreateUser(s.user); err != nil {
		s.logger.Fatal().Msgf("Error creating user: %v", err)
	}

	if s.config.Docker.Enabled {
		if err := system.AddUserToDockerSocketGroup(s.user.GetUsername(), config.DOCKER_SOCKET_PATH); err != nil {
			s.logger.Error().Msgf("Error adding user to docker socket group: %v", err)
		} else {
			s.logger.Info().Msgf("User %s added to docker socket group", s.user.GetUsername())
		}
	}

	if s.config.Docker.Enabled && s.config.Docker.CreateDockerSockSymlink {
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

	err = s.runInitScripts(s.config.InitScriptsDir, s.user, func() {
		s.logger.Info().Msg("Init scripts finished, running auto-start apps")

		appMgr := s.appManager
		if appMgr == nil {
			s.logger.Warn().Msg("apps are not enabled, skipping auto-start")
			return
		}

		if appMgr.Apps() == nil || len(*appMgr.Apps()) == 0 {
			s.logger.Info().Msg("No apps configured, skipping auto-start")
			return
		}

		bg := context.Background()
		for name, app := range *appMgr.Apps() {
			if !app.AutoStart {
				continue
			}
			s.logger.Info().Msgf("Auto-starting app %s", name)
			if err := appMgr.InstallAndStart(bg, name); err != nil {
				s.logger.Error().Msgf("Failed to autostart app %s: %v", name, err)
			}
		}
	})
	if err != nil {
		s.logger.Error().Msgf("Failed to run init scripts: %v", err)
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
		if err := s.grpcService.Serve(ctx); err != nil {
			s.logger.Error().Msgf("gRPC service error: %v", err)
		}
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
		err := s.sysInfo.Collect(ctx, 10)
		if err != nil {
			s.logger.Error().Msgf("system info collection error: %v", err)
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

	shutdownReason := make(chan string, 1)
	if s.jwtVerifier != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.watchIdentity(ctx, shutdownReason)
		}()
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGTERM, syscall.SIGINT)

	select {
	case sig := <-sigChan:
		s.logger.Info().Msgf("Received signal: %s. Initiating shutdown...", sig)
	case reason := <-shutdownReason:
		s.logger.Warn().Msgf("Initiating shutdown: %s", reason)
	}

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

// RunInitScripts runs the initialization scripts
func (s *Server) runInitScripts(
	scriptsDir string,
	user *models.User,
	onComplete func(),
) error {

	s.logger.Info().Msgf("Running init scripts, scriptsDir: %s", scriptsDir)
	if _, err := os.Stat(scriptsDir); os.IsNotExist(err) {
		return fmt.Errorf("invalid init scripts directory: %s", scriptsDir)
	}

	flagDir := fmt.Sprintf(FLAG_DIR_TEMPLATE, user.GetHomeDir())
	if err := os.MkdirAll(flagDir, 0o755); err != nil {
		return fmt.Errorf("failed to create flag directory: %s", flagDir)
	}

	scripts, err := filepath.Glob(filepath.Join(scriptsDir, "__init_*"))
	if err != nil {
		return fmt.Errorf("failed to list init scripts: %s", scriptsDir)
	}

	s.logger.Info().Msgf("Running %d init scripts in background.", len(scripts))
	go func() {
		for _, scriptPath := range scripts {
			s.logger.Info().Msgf("Running %s.", scriptPath)
			err := s.runScriptHelper(scriptsDir, scriptPath, flagDir, []string{})
			if err != nil {
				s.logger.Error().Msgf("Failed to run init script %s: %v", scriptPath, err)
			}
		}
		s.logger.Info().Msg("All init scripts completed.")
		if onComplete != nil {
			onComplete()
		}
	}()

	return nil
}

// runScriptHelper executes a script with flag handling
func (s *Server) runScriptHelper(scriptsDir, scriptPath string, flagDir string, envVars []string) error {
	scriptName := filepath.Base(scriptPath)

	flagFile := ""
	if strings.Contains(scriptName, "__flag") {
		flagFile = filepath.Join(flagDir, scriptName)
		if _, err := os.Stat(flagFile); err == nil {
			s.logger.Info().Msgf("Flag file exists for %s. Skipping execution.", scriptName)
			return nil
		}
	}

	return s.runScript(scriptsDir, scriptName, flagFile, envVars)
}

// runScript executes a script
func (s *Server) runScript(scriptsDir, scriptName, flagFile string, envVars []string) error {
	cmd := exec.Command("/bin/bash", "-l", "-c", fmt.Sprintf("%s/%s", scriptsDir, scriptName))
	cmd.Env = system.CreateEnvVars(envVars, s.user.GetHomeDir())
	cmd.Dir = s.user.GetHomeDir()

	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true, // create a new process group
		Credential: &syscall.Credential{
			Uid:    s.user.GetUID(),
			Gid:    s.user.GetGID(),
			Groups: system.GetSupplementalGroups(s.user.GetUsername()),
		},
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to get stdout pipe for script %s: %w", scriptName, err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to get stderr pipe for script %s: %w", scriptName, err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start script %s: %w", scriptName, err)
	}

	s.procWatcher.AddPIDIgnoreTerminate(cmd.Process.Pid)

	scannerOut := bufio.NewScanner(stdout)
	scannerErr := bufio.NewScanner(stderr)

	go func() {
		for scannerOut.Scan() {
			s.logger.Debug().Msgf("script=%s, msg=%s", scriptName, scannerOut.Text())
		}
	}()
	go func() {
		for scannerErr.Scan() {
			s.logger.Debug().Msgf("script=%s, msg=%s", scriptName, scannerErr.Text())
		}
	}()

	if err := cmd.Wait(); err != nil {
		s.logger.Error().Msgf("Failed to wait for script %s: %v", scriptName, err)
	}
	s.checkScriptState(cmd, flagFile, scriptName)
	return nil
}

// checkScriptState checks the state of a script after it has finished executing.
func (s *Server) checkScriptState(cmd *exec.Cmd, flagFile string, scriptName string) {
	status := cmd.ProcessState.ExitCode()
	if status == 0 {
		if flagFile != "" {
			if err := os.WriteFile(flagFile, []byte{}, 0644); err == nil {
				s.logger.Info().Msgf("The script %s completed successfully. Flag file created at %s.", scriptName, flagFile)
			} else {
				s.logger.Error().Msgf("Failed to create flag file for background script %s: %v", scriptName, err)
			}
		} else {
			s.logger.Info().Msgf("The script %s completed successfully.", scriptName)
		}
	} else {
		s.logger.Error().Msgf("The script %s failed with exit status %d.", scriptName, status)
	}
}
