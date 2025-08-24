package server

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

	"github.com/k8shell-io/k8shelld/internal/log"
	"github.com/k8shell-io/k8shelld/pkg/api/k8shelldpb"
	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	flagDirTemplate = "%s/.k8shell/flags"
)

var ScriptsPIDs = []int{}
var ScriptsPIDsMutex sync.Mutex

// InitServiceServer is the service that handles the init GRPC service server
type InitServiceServer struct {
	grpcApi    *GRPCApiService
	user       User
	envVars    []string
	scriptsDir string
	logger     *zerolog.Logger
	k8shelldpb.UnimplementedInitServiceServer
}

// NewInitServiceServer creates a new InitServiceServer
func NewInitServiceServer(grpcapi *GRPCApiService) *InitServiceServer {
	return &InitServiceServer{
		grpcApi:    grpcapi,
		user:       grpcapi.user,
		scriptsDir: grpcapi.initScriptsDir,
		logger:     log.NewLogger("init-scripts"),
	}
}

func (s *InitServiceServer) RunInitScripts(ctx context.Context,
	req *k8shelldpb.InitRequest) (*k8shelldpb.InitResponse, error) {

	s.envVars = req.SetEnvVars

	s.logger.Info().Msg("Running init scripts...")
	if _, err := os.Stat(s.scriptsDir); os.IsNotExist(err) {
		s.logger.Error().Msgf("No init scripts found in %s directory", s.scriptsDir)
		return &k8shelldpb.InitResponse{Message: "No init scripts found."},
			status.Error(codes.Internal, "No init scripts found")
	}

	flagDir := fmt.Sprintf(flagDirTemplate, s.user.HomeDir)
	if err := os.MkdirAll(flagDir, 0755); err != nil {
		s.logger.Error().Msgf("Failed to create flag directory: %v", err)
		return &k8shelldpb.InitResponse{Message: "Failed to create init scripts flag directory."},
			status.Error(codes.Internal, "Failed to create init scripts flag directory")
	}

	scripts, err := filepath.Glob(filepath.Join(s.scriptsDir, "__init_*"))
	if err != nil {
		s.logger.Error().Msgf("Failed to list init scripts: %v", err)
		return &k8shelldpb.InitResponse{Message: "Failed to list init scripts."},
			status.Error(codes.Internal, "Failed to list init scripts")
	}

	s.logger.Info().Msgf("Running %d init scripts in background.", len(scripts))
	go func() {
		for _, scriptPath := range scripts {
			s.logger.Info().Msgf("Running %s.", scriptPath)
			err := s.runScriptHelper(scriptPath, flagDir)
			if err != nil {
				s.logger.Error().Msgf("Failed to run init script %s: %v", scriptPath, err)
			}
		}
		s.logger.Info().Msg("All init scripts completed.")
	}()

	return &k8shelldpb.InitResponse{Message: "Init scripts started."}, nil
}

func (s *InitServiceServer) checkScriptState(cmd *exec.Cmd, flagFile string, scriptName string) {
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

// runScriptHelper executes a script with flag handling
func (s *InitServiceServer) runScriptHelper(scriptPath string, flagDir string) error {
	scriptName := filepath.Base(scriptPath)

	flagFile := ""
	if strings.Contains(scriptName, "__flag") {
		flagFile = filepath.Join(flagDir, scriptName)
		if _, err := os.Stat(flagFile); err == nil {
			s.logger.Info().Msgf("Flag file exists for %s. Skipping execution.", scriptName)
			return nil
		}
	}

	return s.runScript(scriptName, flagFile)
}

// runScript executes a script
func (s *InitServiceServer) runScript(scriptName, flagFile string) error {
	cmd := exec.Command("/bin/bash", "-l", "-c", fmt.Sprintf("%s/%s", s.scriptsDir, scriptName))
	cmd.Env = CreateEnvVars(s.envVars, s.grpcApi.user.HomeDir)
	cmd.Dir = s.grpcApi.user.HomeDir

	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true, // create a new process group
		Credential: &syscall.Credential{
			Uid:    uint32(s.grpcApi.user.Uid),
			Gid:    uint32(s.grpcApi.user.Gid),
			Groups: getSupplementalGroups(s.grpcApi.user.Username),
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

	AddPIDIgnoreTerminate(cmd.Process.Pid)

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
