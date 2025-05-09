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
)

const (
	flagDirTemplate = "%s/.k8shell/flags"
)

type InitScripts struct {
	user       MainUser
	scriptsDir string
	mu         sync.Mutex
	pids       []int
	logger     *Logger
}

var ScriptsPIDs = []int{}
var ScriptsPIDsMutex sync.Mutex

func NewInitScripts(user MainUser, scriptsDir string) *InitScripts {
	return &InitScripts{
		user:       user,
		scriptsDir: scriptsDir,
		logger:     NewLogger("init-scripts"),
		pids:       []int{},
		mu:         sync.Mutex{},
	}
}

func NewCommand(cmdstr string, user MainUser) *exec.Cmd {
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
				is.logger.Info("The script %s completed successfully. Flag file created at %s.", scriptName, flagFile)
			} else {
				is.logger.Error("Failed to create flag file for background script %s: %v", scriptName, err)
			}
		} else {
			is.logger.Info("The script %s completed successfully.", scriptName)
		}
	} else {
		is.logger.Error("The script %s failed with exit status %d.", scriptName, status)
	}
}

// runBackgroundScript executes a script in the background
func (is *InitScripts) runBackgroundScript(user MainUser, scriptDir, scriptName, flagFile string) {
	cmd := NewCommand(fmt.Sprintf("%s/%s &>/tmp/%s.out", scriptDir, scriptName, scriptName), user)
	err := cmd.Start()
	AddPIDIgnoreTerminate(cmd.Process.Pid)

	if err != nil {
		is.logger.Error("Failed to start background script %s: %v", scriptName, err)
		return
	}
	cmd.Wait()
	is.checkScriptState(cmd, flagFile, scriptName)
}

// runForegroundScript executes a script in the foreground
func (is *InitScripts) runForegroundScript(user MainUser, scriptDir, scriptName, flagFile string) {
	cmd := NewCommand(fmt.Sprintf("%s/%s", scriptDir, scriptName), user)

	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	scannerOut := bufio.NewScanner(stdout)
	scannerErr := bufio.NewScanner(stderr)

	go func() {
		for scannerOut.Scan() {
			is.logger.Info("%s", scannerOut.Text())
		}
	}()
	go func() {
		for scannerErr.Scan() {
			is.logger.Error("%s", scannerErr.Text())
		}
	}()

	cmd.Start()
	AddPIDIgnoreTerminate(cmd.Process.Pid)
	cmd.Wait()
	is.checkScriptState(cmd, flagFile, scriptName)
}

func (is *InitScripts) Run() {
	is.logger.Info("Running k8shell workspace init scripts in %s", is.scriptsDir)
	if _, err := os.Stat(is.scriptsDir); os.IsNotExist(err) {
		is.logger.Info("No init scripts found in %s directory", is.scriptsDir)
		return
	}

	flagDir := fmt.Sprintf(flagDirTemplate, is.user.HomeDir)
	if err := os.MkdirAll(flagDir, 0755); err != nil {
		is.logger.Fatal("Failed to create flag directory: %v", err)
	}

	scripts, err := filepath.Glob(filepath.Join(is.scriptsDir, "__init_*"))
	if err != nil {
		is.logger.Error("Failed to list init scripts: %v", err)
		return
	}

	for _, scriptPath := range scripts {
		scriptName := filepath.Base(scriptPath)
		is.logger.Info("Processing %s", scriptName)

		flagFile := ""
		if strings.Contains(scriptName, "__flag") {
			flagFile = filepath.Join(flagDir, scriptName)
			if _, err := os.Stat(flagFile); err == nil {
				is.logger.Info("Flag file exists for %s. Skipping execution.", scriptName)
				continue
			}
		}

		if strings.HasSuffix(scriptName, "__bg") {
			is.logger.Info("Running %s in background.", scriptName)
			go is.runBackgroundScript(is.user, is.scriptsDir, scriptName, flagFile)
		} else {
			is.logger.Info("Running %s in foreground.", scriptName)
			is.runForegroundScript(is.user, is.scriptsDir, scriptName, flagFile)
		}
	}
	is.logger.Info("All foreground init scripts completed. Background scripts may still be running.")
}
