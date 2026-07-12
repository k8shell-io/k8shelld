// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

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
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/k8shell-io/common/pkg/authz"
	commonmodels "github.com/k8shell-io/common/pkg/models"
	"github.com/k8shell-io/k8shelld/internal/apiclient"
	"github.com/k8shell-io/k8shelld/internal/apps"
	"github.com/k8shell-io/k8shelld/internal/config"
	"github.com/k8shell-io/k8shelld/internal/grpc"
	"github.com/k8shell-io/k8shelld/internal/logger"
	"github.com/k8shell-io/k8shelld/internal/models"
	"github.com/k8shell-io/k8shelld/internal/system"
	"github.com/rs/zerolog"
)

const (
	FLAG_DIR_TEMPLATE                   = "%s/.k8shell/flags"
	SystemInfoCollectionIntervalSeconds = 30
)

type Server struct {
	logger      *zerolog.Logger
	testMode    bool
	user        *models.User
	config      *config.Config
	blueprint   *commonmodels.Blueprint
	username    string
	workspace   string
	restService *RESTService
	grpcService *grpc.GRPCService
	procWatcher *system.ProcessWatcher
	apiClientx  *apiclient.Client
	pprof       bool
	sysInfo     *system.SystemInfo
	appManager  *apps.AppManager
	jwtVerifier *authz.JWTVerifier
	initTracker *models.InitTracker
}

func NewServer(cfg *config.Config, restApiUnixSocketPath string, testMode bool) (*Server, error) {

	var apiClient *apiclient.Client
	if cfg.System.ApiServer.Enabled {
		if cfg.System.ApiServer.Address == "" {
			return nil, fmt.Errorf("api server is enabled but address is empty")
		}
		if strings.TrimSpace(os.Getenv(apiclient.PATTokenEnv)) == "" {
			return nil, fmt.Errorf("api server is enabled but %s is not set", apiclient.PATTokenEnv)
		}
		apiClient = apiclient.New(cfg.System.ApiServer.Address)
	}

	jwtVerifier, err := newJWTVerifier()
	if err != nil {
		return nil, fmt.Errorf("error creating JWT verifier: %v", err)
	}

	s := &Server{
		logger:      logger.NewLogger("k8shelld"),
		testMode:    testMode,
		config:      cfg,
		pprof:       cfg.System.PProf,
		apiClientx:  apiClient,
		jwtVerifier: jwtVerifier,
		initTracker: models.NewInitTracker(),
	}

	bp, err := config.LoadBlueprint(config.BlueprintPath)
	if err != nil {
		s.logger.Warn().Msgf("Failed to load blueprint from %s: %v", config.BlueprintPath, err)
	}
	s.blueprint = bp
	s.sysInfo = system.NewSystemInfo(cfg, bp)

	s.username = os.Getenv("USERNAME")
	if s.username == "" {
		return nil, fmt.Errorf("cannot get the username from USERNAME environment variable")
	}

	s.workspace = os.Getenv("WORKSPACE")
	if s.workspace == "" {
		return nil, fmt.Errorf("cannot get the workspace name from WORKSPACE environment variable")
	}

	err = s.loadIdentity()
	if err != nil {
		return nil, fmt.Errorf("error loading identity: %v", err)
	}

	if !s.testMode {
		s.procWatcher = system.NewProcessWatcher(cfg.TerminateOrphans.Enabled, cfg.ReapZombies.Enabled,
			cfg.TerminateOrphans.CheckInterval, cfg.TerminateOrphans.Exclude)
	} else {
		s.procWatcher = system.NewProcessWatcher(false, false, 0, nil)
	}

	if s.blueprint != nil && s.blueprint.EnableApps {
		s.appManager, err = apps.NewAppManager(config.BlueprintApps(s.blueprint.Apps), s.user, s.procWatcher, s.testMode)
		if err != nil {
			return nil, fmt.Errorf("error creating App Manager: %v", err)
		}
	}

	s.grpcService, err = grpc.NewGRPCService(cfg, s.blueprint, s.user, s.jwtVerifier,
		s.procWatcher, s.apiClientx, s.appManager, s.sysInfo)
	if err != nil {
		return nil, fmt.Errorf("error creating GRPC API: %v", err)
	}
	s.grpcService.SetInitTracker(s.initTracker)

	s.restService, err = NewRESTService(restApiUnixSocketPath, s.user, s)
	if err != nil {
		return nil, fmt.Errorf("error creating REST API: %v", err)
	}

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

	s.setupToolWrappers()

	if err := system.CreateUser(s.user); err != nil {
		s.logger.Fatal().Msgf("Error creating user: %v", err)
	}

	s.setupCredentialHelpers()

	if s.blueprint != nil && s.blueprint.Podman.Enabled {
		uid := int(s.user.GetUID())
		gid := int(s.user.GetGID())
		go func() {
			const (
				maxWait      = 30 * time.Second
				pollInterval = 500 * time.Millisecond
			)
			deadline := time.Now().Add(maxWait)
			for time.Now().Before(deadline) {
				if _, err := os.Lstat(config.PODMAN_SOCKET_PATH); err == nil {
					if err := os.Chown(config.PODMAN_SOCKET_PATH, uid, gid); err != nil {
						s.logger.Error().Msgf("Error chowning podman socket %s: %v", config.PODMAN_SOCKET_PATH, err)
					} else {
						s.logger.Info().Msgf("Podman socket %s ownership changed to UID %d GID %d",
							config.PODMAN_SOCKET_PATH, uid, gid)
					}
					return
				}
				time.Sleep(pollInterval)
			}
			s.logger.Warn().Msgf("Podman socket %s not found after %s, skipping chown",
				config.PODMAN_SOCKET_PATH, maxWait)
		}()
	}

	if s.blueprint != nil && s.blueprint.Podman.Enabled && s.blueprint.Podman.CreateDockerSockSymlink {
		if _, err := os.Lstat(config.DOCKER_SOCKET_SYMLINK); err != nil {
			if err := os.Symlink(config.PODMAN_SOCKET_PATH, config.DOCKER_SOCKET_SYMLINK); err != nil {
				s.logger.Error().Msgf("Error creating docker socket symlink: %v", err)
			} else {
				s.logger.Info().Msgf("Created Docker socket symlink: %s -> %s",
					config.DOCKER_SOCKET_SYMLINK, config.PODMAN_SOCKET_PATH)
			}
		} else {
			s.logger.Warn().Msgf("Docker socket symlink already exists: %s", config.DOCKER_SOCKET_SYMLINK)
		}
	}

	runErr := s.runInitScripts(config.InitScriptsDir, s.user, func() {
		s.logger.Info().Msg("Init scripts finished, running auto-start apps")

		appMgr := s.appManager
		if appMgr == nil {
			s.logger.Warn().Msg("apps are not enabled, skipping auto-start")
			return
		}

		if appMgr.Apps() == nil || len(appMgr.Apps()) == 0 {
			s.logger.Info().Msg("No apps configured, skipping auto-start")
			return
		}

		bg := context.Background()
		for name, app := range appMgr.Apps() {
			if !app.AutoStart {
				continue
			}
			s.logger.Info().Msgf("Auto-starting app %s", name)
			if err := appMgr.InstallAndStart(bg, name); err != nil {
				s.logger.Error().Msgf("Failed to autostart app %s: %v", name, err)
			}
		}
	})
	if runErr != nil {
		s.logger.Error().Msgf("Failed to run init scripts: %v", runErr)
	}

	return nil
}

func (s *Server) Serve() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := s.grpcService.Serve(ctx); err != nil {
			s.logger.Error().Msgf("gRPC service error: %v", err)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		s.restService.Serve(ctx)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		s.procWatcher.Run(ctx)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		err := s.sysInfo.Collect(ctx, SystemInfoCollectionIntervalSeconds)
		if err != nil {
			s.logger.Error().Msgf("system info collection error: %v", err)
		}
	}()

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

	sort.Strings(scripts)

	scriptNames := make([]string, 0, len(scripts))
	for _, sp := range scripts {
		scriptNames = append(scriptNames, filepath.Base(sp))
	}
	s.initTracker.RegisterAll(scriptNames)

	s.logger.Info().Msgf("Running %d init scripts sequentially in background.", len(scripts))
	go func() {
		for _, sp := range scripts {
			s.logger.Info().Msgf("Running %s.", sp)
			if err := s.runScriptHelper(scriptsDir, sp, flagDir, []string{}); err != nil {
				s.logger.Error().Msgf("Failed to run init script %s: %v", sp, err)
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

	flagFile := filepath.Join(flagDir, scriptName)
	if _, err := os.Stat(flagFile); err == nil {
		s.logger.Info().Msgf("Flag file exists for %s. Skipping execution.", scriptName)
		s.initTracker.SetCompleted(scriptName, false)
		return nil
	}

	s.initTracker.SetRunning(scriptName)
	return s.runScript(scriptsDir, scriptName, flagFile, envVars)
}

// scriptInterpreter returns the interpreter for a script by reading its shebang line.
// Falls back to "/bin/sh" when no shebang is present.
func scriptInterpreter(scriptPath string) string {
	f, err := os.Open(scriptPath)
	if err != nil {
		return "/bin/sh"
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	if scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "#!") {
			interp := strings.TrimSpace(line[2:])
			if interp != "" {
				return interp
			}
		}
	}
	return "/bin/sh"
}

// runScript executes a script
func (s *Server) runScript(scriptsDir, scriptName, flagFile string, envVars []string) error {
	scriptPath := filepath.Join(scriptsDir, scriptName)

	interp := scriptInterpreter(scriptPath)
	cmd := exec.Command(interp, scriptPath)
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

	unlockCreation := s.procWatcher.LockForCreation()
	if err := cmd.Start(); err != nil {
		unlockCreation()
		return fmt.Errorf("failed to start script %s: %w", scriptName, err)
	}

	s.procWatcher.AddPIDIgnoreTerminate(cmd.Process.Pid)
	unlockCreation()

	scannerOut := bufio.NewScanner(stdout)
	scannerErr := bufio.NewScanner(stderr)

	go func() {
		for scannerOut.Scan() {
			s.logger.Debug().Msgf("script=%s, msg=%s", scriptName, scannerOut.Text())
		}
		if err := scannerOut.Err(); err != nil {
			s.logger.Error().Msgf("script=%s, failed to scan stdout: %v", scriptName, err)
		}
	}()
	go func() {
		for scannerErr.Scan() {
			s.logger.Debug().Msgf("script=%s, msg=%s", scriptName, scannerErr.Text())
		}
		if err := scannerErr.Err(); err != nil {
			s.logger.Error().Msgf("script=%s, failed to scan stderr: %v", scriptName, err)
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
	hasError := status != 0
	s.initTracker.SetCompleted(scriptName, hasError)
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
