package server

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/k8shell-io/k8shelld/internal/log"
	"github.com/rs/zerolog"
)

const (
	flagDirTemplate = "%s/.k8shell/flags"
)

type InitScripts struct {
	user       User
	scriptsDir string
	mu         sync.Mutex
	pids       []int
	logger     *zerolog.Logger
}

var ScriptsPIDs = []int{}
var ScriptsPIDsMutex sync.Mutex

func NewInitScripts(user User, scriptsDir string) *InitScripts {
	return &InitScripts{
		user:       user,
		scriptsDir: scriptsDir,
		logger:     log.NewLogger("init-scripts"),
		pids:       []int{},
		mu:         sync.Mutex{},
	}
}

func NewCommand(cmdstr string, user User) *exec.Cmd {
	cmd := exec.Command("/bin/sh", "-c", cmdstr)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{
			Uid: uint32(user.Uid),
			Gid: uint32(user.Gid),
		},
	}

	newEnv := []string{}
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "HOME=") {
			newEnv = append(newEnv, fmt.Sprintf("HOME=%s", user.HomeDir))
			continue
		}
		newEnv = append(newEnv, e)
	}
	cmd.Env = append(cmd.Env, newEnv...)
	return cmd
}

func (is *InitScripts) checkScriptState(cmd *exec.Cmd, flagFile string, scriptName string) {
	status := cmd.ProcessState.ExitCode()
	if status == 0 {
		if flagFile != "" {
			if err := os.WriteFile(flagFile, []byte{}, 0644); err == nil {
				is.logger.Info().Msgf("The script %s completed successfully. Flag file created at %s.", scriptName, flagFile)
			} else {
				is.logger.Error().Msgf("Failed to create flag file for background script %s: %v", scriptName, err)
			}
		} else {
			is.logger.Info().Msgf("The script %s completed successfully.", scriptName)
		}
	} else {
		is.logger.Error().Msgf("The script %s failed with exit status %d.", scriptName, status)
	}
}

func (is *InitScripts) Run() {
	is.logger.Info().Msgf("Running k8shell workspace init scripts in %s", is.scriptsDir)
	if _, err := os.Stat(is.scriptsDir); os.IsNotExist(err) {
		is.logger.Info().Msgf("No init scripts found in %s directory", is.scriptsDir)
		return
	}

	flagDir := fmt.Sprintf(flagDirTemplate, is.user.HomeDir)
	if err := os.MkdirAll(flagDir, 0755); err != nil {
		is.logger.Fatal().Msgf("Failed to create flag directory: %v", err)
	}

	scripts, err := filepath.Glob(filepath.Join(is.scriptsDir, "__init_*"))
	if err != nil {
		is.logger.Error().Msgf("Failed to list init scripts: %v", err)
		return
	}

	for _, scriptPath := range scripts {
		foreground := !strings.HasSuffix(scriptPath, "__bg")
		if foreground {
			is.runScriptHelper(scriptPath, flagDir, true)
		}
	}

	for _, scriptPath := range scripts {
		background := strings.HasSuffix(scriptPath, "__bg")
		if background {
			is.runScriptHelper(scriptPath, flagDir, false)
		}
	}

	is.logger.Info().Msgf("All foreground init scripts completed. Background scripts may still be running.")
}

// runScriptHelper executes a script with flag handling
func (is *InitScripts) runScriptHelper(scriptPath string, flagDir string, foreground bool) {
	scriptName := filepath.Base(scriptPath)
	is.logger.Info().Msgf("Running script: %s", scriptName)

	flagFile := ""
	if strings.Contains(scriptName, "__flag") {
		flagFile = filepath.Join(flagDir, scriptName)
		if _, err := os.Stat(flagFile); err == nil {
			is.logger.Info().Msgf("Flag file exists for %s. Skipping execution.", scriptName)
			return
		}
	}

	if foreground {
		is.logger.Info().Msgf("Running %s in foreground.", scriptName)
		is.runScript(is.user, is.scriptsDir, scriptName, flagFile)
	} else {
		is.logger.Info().Msgf("Running %s in background.", scriptName)
		go is.runScript(is.user, is.scriptsDir, scriptName, flagFile)
	}
}

// runScript executes a script
func (is *InitScripts) runScript(user User, scriptDir, scriptName, flagFile string) {
	cmd := NewCommand(fmt.Sprintf("%s/%s", scriptDir, scriptName), user)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		is.logger.Error().Err(err).Msgf("Failed to get stdout pipe for script %s", scriptName)
		return
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		is.logger.Error().Err(err).Msgf("Failed to get stderr pipe for script %s", scriptName)
		return
	}

	if err := cmd.Start(); err != nil {
		is.logger.Error().Err(err).Msgf("Failed to start script %s", scriptName)
		return
	}

	AddPIDIgnoreTerminate(cmd.Process.Pid)

	scannerOut := bufio.NewScanner(stdout)
	scannerErr := bufio.NewScanner(stderr)

	go func() {
		for scannerOut.Scan() {
			is.logger.Debug().Msgf("out: script=%s, msg=%s", scriptName, scannerOut.Text())
		}
	}()
	go func() {
		for scannerErr.Scan() {
			is.logger.Debug().Msgf("err: script=%s, msg=%s", scriptName, scannerErr.Text())
		}
	}()

	cmd.Wait()
	is.checkScriptState(cmd, flagFile, scriptName)
}
