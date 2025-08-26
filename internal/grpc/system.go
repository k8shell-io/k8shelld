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

	"github.com/k8shell-io/k8shelld/internal/config"
	"github.com/k8shell-io/k8shelld/internal/log"
	"github.com/k8shell-io/k8shelld/internal/system"
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
		logger:         log.NewLogger("grpc-system"),
		initScriptsRun: false,
	}
}

// Handshake handles the handshake request
func (s *SystemServiceServer) Handshake(ctx context.Context,
	req *k8shelldpb.HandshakeRequest) (*k8shelldpb.HandshakeResponse, error) {
	s.handshakeMu.Lock()
	defer s.handshakeMu.Unlock()

	if s.grpcApi.user.Username != req.User.Username {
		return nil, status.Error(codes.PermissionDenied, "user name mismatch")
	}

	if uint32(s.grpcApi.user.Uid) != req.User.Uid {
		return nil, status.Error(codes.PermissionDenied, "user uid mismatch")
	}

	if uint32(s.grpcApi.user.Gid) != req.User.Gid {
		return nil, status.Error(codes.PermissionDenied, "user gid mismatch")
	}

	s.grpcApi.user.UserToken = req.User.UserToken

	if !s.initScriptsRun {
		s.RunInitScripts(ctx, s.grpcApi.initScriptsDir, s.grpcApi.user, req.EnvVars)
		s.initScriptsRun = true
	}

	return &k8shelldpb.HandshakeResponse{
		Accepted:      true,
		ServerVersion: fmt.Sprintf("%s-%s", config.K8SHELLD_VERSION, config.K8SHELLD_COMMIT),
	}, nil
}

// RunInitScripts runs the initialization scripts
func (s *SystemServiceServer) RunInitScripts(ctx context.Context, scriptsDir string,
	user system.User, envVars []string) error {

	s.logger.Info().Msg("Running init scripts...")
	if _, err := os.Stat(scriptsDir); os.IsNotExist(err) {
		return fmt.Errorf("invalid init scripts directory: %s", scriptsDir)
	}

	flagDir := fmt.Sprintf(flagDirTemplate, user.HomeDir)
	if err := os.MkdirAll(flagDir, 0755); err != nil {
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
	cmd.Env = system.CreateEnvVars(envVars, s.grpcApi.user.HomeDir)
	cmd.Dir = s.grpcApi.user.HomeDir

	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true, // create a new process group
		Credential: &syscall.Credential{
			Uid:    uint32(s.grpcApi.user.Uid),
			Gid:    uint32(s.grpcApi.user.Gid),
			Groups: system.GetSupplementalGroups(s.grpcApi.user.Username),
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

	cmd.Wait()
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
