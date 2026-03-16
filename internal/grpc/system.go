package grpc

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/k8shell-io/k8shelld/internal/config"
	"github.com/k8shell-io/k8shelld/internal/logger"
	"github.com/k8shell-io/k8shelld/internal/models"
	"github.com/k8shell-io/k8shelld/internal/system"
	"github.com/k8shell-io/k8shelld/pkg/api"
	"github.com/k8shell-io/k8shelld/pkg/api/k8shelldpb"
	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	flagDirTemplate = "%s/.k8shell/flags"
)

// SystemServiceServer is the gRPC server for the system service
type SystemServiceServer struct {
	grpcApi        *GRPCService
	logger         *zerolog.Logger
	initScriptsRun bool
	handshakeMu    sync.Mutex
	k8shelldpb.UnimplementedSystemServiceServer
}

// NewSystemServiceServer creates a new SystemServiceServer
func NewSystemServiceServer(grpcapi *GRPCService) *SystemServiceServer {
	return &SystemServiceServer{
		grpcApi:        grpcapi,
		logger:         logger.NewLogger("grpc-system"),
		initScriptsRun: false,
	}
}

// Handshake handles the handshake request
func (s *SystemServiceServer) Handshake(ctx context.Context,
	req *k8shelldpb.HandshakeRequest) (*k8shelldpb.HandshakeResponse, error) {
	s.handshakeMu.Lock()
	defer s.handshakeMu.Unlock()

	s.logger.Info().Msgf("Received handshake from user: %s, uid: %d, gid: %d",
		req.User.Username, req.User.Uid, req.User.Gid)

	if s.grpcApi.Config.User.Username != req.User.Username {
		return nil, status.Error(codes.PermissionDenied, "user name mismatch")
	}

	if s.grpcApi.Config.User.Uid != req.User.Uid {
		return nil, status.Error(codes.PermissionDenied, "user uid mismatch")
	}

	if s.grpcApi.Config.User.Gid != req.User.Gid {
		return nil, status.Error(codes.PermissionDenied, "user gid mismatch")
	}

	if req.User.UserToken == "" {
		s.logger.Warn().Msg("Empty user token received in handshake")
	} else {
		var tokenPreview string
		if len(req.User.UserToken) >= 4 {
			tokenPreview = req.User.UserToken[:4]
		} else {
			tokenPreview = "****"
		}

		s.logger.Debug().Msgf("User token received in handshake: token=***%s", tokenPreview)
		s.grpcApi.Config.User.UserToken = req.User.UserToken
		if s.grpcApi.apiClientx != nil {
			s.grpcApi.apiClientx.UpdateToken(req.User.UserToken)
		}
	}

	if !s.initScriptsRun {
		err := s.RunInitScripts(ctx, s.grpcApi.Config.InitScriptsDir, s.grpcApi.Config.User, req.EnvVars, func() {
			s.logger.Info().Msg("Init scripts finished, running auto-start apps")

			appMgr := s.grpcApi.appManager
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
		s.initScriptsRun = true
	}

	s.logger.Info().Msg("Handshake successful")

	return &k8shelldpb.HandshakeResponse{
		Accepted:      true,
		ServerVersion: fmt.Sprintf("%s-%s", config.K8SHELLD_VERSION, config.K8SHELLD_COMMIT),
	}, nil
}

// RunInitScripts runs the initialization scripts
func (s *SystemServiceServer) RunInitScripts(
	ctx context.Context,
	scriptsDir string,
	user models.User,
	envVars []string,
	onComplete func(),
) error {

	s.logger.Info().Msgf("Running init scripts, scriptsDir: %s, env: %s",
		scriptsDir, strings.Join(envVars, ", "))
	if _, err := os.Stat(scriptsDir); os.IsNotExist(err) {
		return fmt.Errorf("invalid init scripts directory: %s", scriptsDir)
	}

	flagDir := fmt.Sprintf(flagDirTemplate, user.HomeDir)
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
			err := s.runScriptHelper(scriptsDir, scriptPath, flagDir, envVars)
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
func (s *SystemServiceServer) runScriptHelper(scriptsDir, scriptPath string, flagDir string, envVars []string) error {
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
func (s *SystemServiceServer) runScript(scriptsDir, scriptName, flagFile string, envVars []string) error {
	cmd := exec.Command("/bin/bash", "-l", "-c", fmt.Sprintf("%s/%s", scriptsDir, scriptName))
	cmd.Env = system.CreateEnvVars(envVars, s.grpcApi.Config.User.HomeDir)
	cmd.Dir = s.grpcApi.Config.User.HomeDir

	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true, // create a new process group
		Credential: &syscall.Credential{
			Uid:    s.grpcApi.Config.User.Uid,
			Gid:    s.grpcApi.Config.User.Gid,
			Groups: system.GetSupplementalGroups(s.grpcApi.Config.User.Username),
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

	s.grpcApi.procWatcher.AddPIDIgnoreTerminate(cmd.Process.Pid)

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
func (s *SystemServiceServer) checkScriptState(cmd *exec.Cmd, flagFile string, scriptName string) {
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

// SystemInfo returns system metrics + mount usage + docker usage over gRPC.
// Mirrors the REST /sysinfo payload.
func (s *SystemServiceServer) SystemInfo(ctx context.Context,
	_ *k8shelldpb.SystemInfoRequest) (*k8shelldpb.SystemInfoResponse, error) {

	metrics, err := s.grpcApi.sysInfo.GetSystemUsageSnapshot()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get system metrics: %v", err)
	}
	metrics.Users = s.grpcApi.NumSessions()

	mounts, err := s.grpcApi.sysInfo.GetMountUsageSnapshot()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get mount usage: %v", err)
	}

	docker, err := s.grpcApi.sysInfo.GetDockerUsageSnapshot(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get docker usage: %v", err)
	}

	systemInfo := api.SystemInfo{
		Time:   time.Now().Format(time.RFC3339),
		System: metrics,
		Mounts: mounts,
		Docker: docker,
	}

	return api.SystemInfoToProto(&systemInfo), nil
}
