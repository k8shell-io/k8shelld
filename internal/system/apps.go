package system

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/k8shell-io/k8shelld/internal/config"
	"github.com/k8shell-io/k8shelld/internal/logger"
	"github.com/k8shell-io/k8shelld/internal/models"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

const (
	APPS_DIR            = "/var/log/k8shelld/apps"
	VERSION_CMD_TIMEOUT = 10 * time.Second
	APP_STOP_TIMEOUT    = 10 * time.Second
)

// AppState represents the persistent state of an application
type AppState struct {
	InstalledVersion string    `json:"installed_version"`
	LastInstalledAt  time.Time `json:"last_installed_at"`
}

// supervisorState holds the state for a supervisor
type supervisorState struct {
	stopCh       chan struct{}
	restartCount int
	pid          int
}

// AppManager manages the lifecycle of applications defined in the configuration
type AppManager struct {
	apps        *config.Apps
	user        config.User
	stateDir    string
	logger      *zerolog.Logger
	mu          sync.Mutex
	installing  map[string]bool
	testMode    bool
	procWatcher *ProcessWatcher

	supervisors map[string]*supervisorState
}

// NewAppManager creates a new AppManager instance
func NewAppManager(apps *config.Apps, user config.User, procWatcher *ProcessWatcher, stateDir string,
	testMode bool) (*AppManager, error) {
	log := logger.NewLogger("app-manager")

	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}

	return &AppManager{
		apps:        apps,
		user:        user,
		procWatcher: procWatcher,
		stateDir:    stateDir,
		logger:      log,
		testMode:    testMode,
		installing:  make(map[string]bool),
		supervisors: make(map[string]*supervisorState),
	}, nil
}

// ensureStateDir ensures the state directory for the app exists.
func (m *AppManager) ensureAppStateDir(name string) (string, error) {
	stateDir := filepath.Join(m.stateDir, name)
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return "", fmt.Errorf("create state dir for %s: %w", name, err)
	}
	return stateDir, nil
}

// isAppInstalled checks if the app is installed by verifying the binary exists.
func (m *AppManager) isAppInstalled(name string) (bool, error) {
	app, ok := (*m.apps)[name]
	if !ok {
		return false, fmt.Errorf("app %s not found", name)
	}

	env := CreateEnvVars([]string{}, m.user.HomeDir)
	binaryPath := expandEnv(app.Binary, env)
	if binaryPath == "" {
		return false, fmt.Errorf("app binary not specified")
	}

	if _, err := os.Stat(binaryPath); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("stat binary: %w", err)
	}

	return true, nil
}

// appVersion retrieves the installed version of the app by running the version command.
func (m *AppManager) appVersion(ctx context.Context, name string) (string, error) {
	app, ok := (*m.apps)[name]
	if !ok {
		return "", fmt.Errorf("app %s not found", name)
	}

	if len(app.VersionCmd) == 0 || app.VersionRegex == "" {
		return "", fmt.Errorf("version command or version regex not configured for app %s", name)
	}

	env := CreateEnvVars([]string{}, m.user.HomeDir)
	versionCmd := expandEnvSlice(app.VersionCmd, env)

	cmd := exec.CommandContext(ctx, versionCmd[0], versionCmd[1:]...)
	if app.InstallAsRoot && !m.testMode {
		cmd.Env = os.Environ()
		cmd.Dir = "/root"
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Setsid:    true,
			Pdeathsig: 0,
		}
	} else {
		cmd.Env = CreateEnvVars([]string{}, m.user.HomeDir)
		cmd.Dir = m.user.HomeDir
		if !m.testMode {
			cmd.SysProcAttr = &syscall.SysProcAttr{
				Setsid:    true,
				Pdeathsig: 0,
				Credential: &syscall.Credential{
					Uid:    uint32(m.user.Uid),
					Gid:    uint32(m.user.Gid),
					Groups: GetSupplementalGroups(m.user.Username),
				},
			}
		}
	}

	var b bytes.Buffer
	cmd.Stdout = &b
	cmd.Stderr = &b

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start version command: %w", err)
	}

	if !m.testMode {
		m.procWatcher.AddPIDIgnoreTerminate(cmd.Process.Pid)
	}

	timeout := VERSION_CMD_TIMEOUT
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case err := <-done:
		if err != nil {
			return "", fmt.Errorf("version command failed: %w", err)
		}
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		<-done
		return "", fmt.Errorf("version command timed out after %s", timeout)
	}

	out := b.Bytes()
	version, err := m.parseVersion(app.VersionRegex, string(out))
	if err != nil {
		return "", err
	}

	return version, nil
}

// appVersionFromFile reads the installed version of the app from the version file.
func (m *AppManager) appVersionFromFile(name string) (string, error) {
	app, ok := (*m.apps)[name]
	if !ok {
		return "", fmt.Errorf("app %s not found", name)
	}

	appStateDir, err := m.ensureAppStateDir(name)
	if err != nil {
		return "", err
	}
	versionFilePath := filepath.Join(appStateDir, fmt.Sprintf("%s-version.txt", name))
	data, err := os.ReadFile(versionFilePath)
	if err != nil {
		return "", fmt.Errorf("read version file: %w", err)
	}

	version, err := m.parseVersion(app.VersionRegex, string(data))
	if err != nil {
		return "", fmt.Errorf("parse version from file: %w", err)
	}

	return version, nil
}

// writeAppVersionToFile writes the installed version of the app to the version file.
func (m *AppManager) writeAppVersionToFile(name, version string) error {
	appStateDir, err := m.ensureAppStateDir(name)
	if err != nil {
		return err
	}
	versionFilePath := filepath.Join(appStateDir, fmt.Sprintf("%s-version.txt", name))
	if err := os.WriteFile(versionFilePath, []byte(version), 0o644); err != nil {
		return fmt.Errorf("write version file: %w", err)
	}
	return nil
}

func (m *AppManager) ensureAppVersion(name string) (string, error) {
	installed, err := m.isAppInstalled(name)
	if err != nil {
		return "", fmt.Errorf("cannot check if %s is installed: %w", name, err)
	}
	if !installed {
		return "", fmt.Errorf("app %s is not installed", name)
	}

	version, err := m.appVersionFromFile(name)
	if err != nil {
		m.logger.Warn().Msgf("could not read version file for app %s: %v", name, err)
		ctx := context.Background()
		version, err = m.appVersion(ctx, name)
		if err != nil {
			return "", fmt.Errorf("cannot determine version for %s: %w", name, err)
		}
		err = m.writeAppVersionToFile(name, version)
		if err != nil {
			m.logger.Warn().Msgf("could not write version file for app %s: %v", name, err)
		}
	}
	return version, nil
}

func (m *AppManager) parseVersion(versionRegex, str string) (string, error) {
	re, err := regexp.Compile(versionRegex)
	if err != nil {
		return "", fmt.Errorf("invalid versionRegex: %w", err)
	}
	matches := re.FindStringSubmatch(str)
	if len(matches) >= 2 {
		return matches[1], nil
	}
	return "", fmt.Errorf("cannot parse version from string using regex %s: %s", versionRegex, str)
}

// InstallAsync starts installation in the background.
// If an install for this app is already running, it returns an error.
func (m *AppManager) InstallAsync(ctx context.Context, name string, force bool) error {
	_, ok := (*m.apps)[name]
	if !ok {
		return fmt.Errorf("app %s not found", name)
	}

	m.mu.Lock()
	if m.installing[name] {
		m.mu.Unlock()
		return fmt.Errorf("install for %s is already running", name)
	}

	if !force {
		installed, err := m.isAppInstalled(name)
		if err != nil {
			m.mu.Unlock()
			return fmt.Errorf("cannot check if %s is installed: %w", name, err)
		}
		if installed {
			m.mu.Unlock()
			return fmt.Errorf("app %s is already installed", name)
		}
	}

	if _, ok := m.supervisors[name]; ok {
		m.mu.Unlock()
		return fmt.Errorf("cannot install %s while it is running", name)
	}

	m.installing[name] = true
	m.mu.Unlock()

	asyncCtx := context.Background()

	go func() {
		defer func() {
			m.mu.Lock()
			delete(m.installing, name)
			m.mu.Unlock()
		}()

		if err := m.runInstall(asyncCtx, name); err != nil {
			m.logger.Error().Msgf("install of %s failed: %v", name, err)
		} else {
			m.logger.Info().Msgf("install of %s completed successfully", name)
		}
	}()

	return nil
}

// runInstall executes the installation script for the app (synchronous).
func (m *AppManager) runInstall(ctx context.Context, name string) error {
	app, ok := (*m.apps)[name]
	if !ok {
		return fmt.Errorf("app %s not found", name)
	}

	if app.Install == "" {
		return fmt.Errorf("no install script provided for %s", app.Name)
	}

	env := CreateEnvVars([]string{}, m.user.HomeDir)
	installScript := expandEnv(app.Install, env)
	appStateDir, err := m.ensureAppStateDir(name)
	if err != nil {
		return err
	}

	scriptPath := filepath.Join(appStateDir, fmt.Sprintf("%s-install.sh", name))
	if err := os.WriteFile(scriptPath, []byte(installScript), 0o755); err != nil {
		return fmt.Errorf("write install script: %w", err)
	}

	logFile, logPath, err := m.OpenLogFile(name, "install")
	if err != nil {
		log.Error().Err(err).Msg("failed to open install log file")
		return fmt.Errorf("open install log file: %w", err)
	}
	defer logFile.Close()

	m.logger.Debug().Msgf("running install script for %s, output=%s, as_root=%v", name, logPath, app.InstallAsRoot)

	cmd := exec.CommandContext(ctx, "/bin/sh", scriptPath)
	if app.InstallAsRoot && !m.testMode {
		cmd.Env = os.Environ()
		cmd.Dir = "/root"
	} else {
		cmd.Env = CreateEnvVars([]string{}, m.user.HomeDir)
		cmd.Dir = m.user.HomeDir

		if !m.testMode {
			cmd.SysProcAttr = &syscall.SysProcAttr{
				Setsid: true,
				Credential: &syscall.Credential{
					Uid:    uint32(m.user.Uid),
					Gid:    uint32(m.user.Gid),
					Groups: GetSupplementalGroups(m.user.Username),
				},
			}
		}
	}

	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start install script: %w", err)
	}

	if !m.testMode {
		m.procWatcher.AddPIDIgnoreTerminate(cmd.Process.Pid)
	}

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("install script failed: %w", err)
	}

	time.Sleep(1 * time.Second)
	appVersion, err := m.appVersion(ctx, name)
	if err != nil {
		m.logger.Warn().Msgf("could not determine app version after install: %v", err)
		appVersion = "INVALID"
	}

	err = m.writeAppVersionToFile(name, appVersion)
	if err != nil {
		m.logger.Warn().Msgf("could not write app version before install: %v", err)
	}

	return nil
}

// EnsureRunning starts a supervisor for the given app if not already running.
// The supervisor will keep the app running according to its RestartPolicy.
func (m *AppManager) EnsureRunning(ctx context.Context, name string) error {
	app, ok := (*m.apps)[name]
	if !ok {
		return fmt.Errorf("app %s not found", name)
	}

	isInstalled, err := m.isAppInstalled(name)
	if err != nil {
		return fmt.Errorf("cannot check if %s is installed: %w", name, err)
	}
	if !isInstalled {
		return fmt.Errorf("app %s is not installed", name)
	}

	m.mu.Lock()
	if _, ok := m.supervisors[name]; ok {
		m.mu.Unlock()
		return nil
	}

	st := &supervisorState{
		stopCh: make(chan struct{}),
	}
	m.supervisors[name] = st
	m.mu.Unlock()

	go m.superviseApp(name, app, st)

	return nil
}

// Stop asks the supervisor to stop managing the app and kills the process if running.
func (m *AppManager) Stop(ctx context.Context, name string) error {
	_, ok := (*m.apps)[name]
	if !ok {
		return fmt.Errorf("app %s not found", name)
	}

	m.mu.Lock()
	st, ok := m.supervisors[name]
	m.mu.Unlock()

	if !ok {
		return fmt.Errorf("app %s is not running", name)
	}

	close(st.stopCh)

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(APP_STOP_TIMEOUT):
		return fmt.Errorf("timeout waiting for app %s to stop", name)
	}
}

// ListAppStatus returns app status including port, PID and running time, without internal state.
func (m *AppManager) ListAppStatus(ctx context.Context) ([]models.AppStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var res []models.AppStatus

	for name, app := range *m.apps {
		installing := m.installing[name]

		status := models.AppStatus{
			Name:     name,
			Status:   "-",
			Version:  "",
			Port:     app.Listen,
			PID:      0,
			Age:      "",
			Restarts: 0,
		}

		var sup *supervisorState
		if s, ok := m.supervisors[name]; ok {
			sup = s
			status.Restarts = s.restartCount
		}

		version := "N/A"
		installed, err := m.isAppInstalled(name)
		if err != nil {
			m.logger.Warn().Msgf("could not check if app %s is installed: %v", name, err)
			installed = false
		}

		if installed {
			v, err := m.appVersionFromFile(name)
			if err != nil {
				m.logger.Warn().Msgf("could not read version file for app %s: %v", name, err)
				v = "UNKNOWN"
			}
			version = v
		}

		status.Version = version

		if installing {
			status.Status = "INSTALLING"
			res = append(res, status)
			continue
		}

		if !installed {
			status.Status = "N/A"
			res = append(res, status)
			continue
		}

		if sup == nil {
			pid, err := GetPIDListeningOnPort(app.Listen)
			if err != nil {
				m.logger.Warn().Msgf("Could not get PID for app %s, port %d: %v", name, app.Listen, err)
				status.Status = "STOPPED"
				res = append(res, status)
				continue
			}

			if pid != 0 {
				status.Status = "INVALID"
				res = append(res, status)
				continue
			}

			status.Status = "STOPPED"
			res = append(res, status)
			continue
		}

		if sup.pid == 0 {
			status.Status = "PENDING"
			res = append(res, status)
			continue
		}

		status.PID = sup.pid
		status.Status = "RUNNING"

		if dur, err := GetProcessRunningTime(sup.pid); err == nil {
			status.Age = dur.Truncate(time.Second).String()
		}

		res = append(res, status)
	}

	return res, nil
}

// IsInstalling reports whether an install is currently running for the app.
func (m *AppManager) IsInstalling(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.installing[name]
}

// IsRunning reports whether the app is currently supervised and has a running PID.
func (m *AppManager) IsRunning(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	s, ok := m.supervisors[name]
	if !ok {
		return false
	}
	return s.pid != 0
}

// GetLogFilePath returns a new log file path for the app of the given type.
func (m *AppManager) OpenLogFile(name string, logType string) (*os.File, string, error) {
	appStateDir, err := m.ensureAppStateDir(name)
	if err != nil {
		return nil, "", err
	}

	timestamp := time.Now()
	logPath := filepath.Join(appStateDir,
		fmt.Sprintf("%s-%s-%s.out", name, logType, timestamp.Format("20060102-150405")))

	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, "", err
	}

	return logFile, logPath, nil
}

// GetLastlLog returns the contents of the most recent log of the given type for the app.
// If no log exists, it returns empty string and no error.
func (m *AppManager) GetLastLog(name string, logType string) (string, error) {
	path, err := m.GetLastLogPath(name, logType)
	if err != nil {
		return "", err
	}
	if path == "" {
		return "", nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s log: %w", logType, err)
	}
	return string(data), nil
}

// GetLastLogPath returns the path of the most recent log for the app of the given type.
// If no log exists, it returns an empty string and no error.
func (m *AppManager) GetLastLogPath(name string, logType string) (string, error) {
	if logType != "install" && logType != "app" {
		return "", fmt.Errorf("invalid log type: %s", logType)
	}

	appStateDir, err := m.ensureAppStateDir(name)
	if err != nil {
		return "", err
	}

	entries, err := os.ReadDir(appStateDir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("read %s state dir: %w", logType, err)
	}

	var latestName string
	var latestTime time.Time

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if !strings.HasPrefix(n, fmt.Sprintf("%s-%s-", name, logType)) || !strings.HasSuffix(n, ".out") {
			continue
		}

		ts := strings.TrimSuffix(strings.TrimPrefix(n, fmt.Sprintf("%s-%s-", name, logType)), ".out")
		t, err := time.Parse("20060102-150405", ts)
		if err != nil {
			continue
		}
		if t.After(latestTime) {
			latestTime = t
			latestName = n
		}
	}

	if latestName == "" {
		return "", nil
	}
	return filepath.Join(appStateDir, latestName), nil
}

// superviseApp runs the supervisor loop for the app.
func (m *AppManager) superviseApp(name string, app *config.AppSpec, st *supervisorState) {
	log := m.logger.With().Str("app", name).Logger()

	policy := strings.ToLower(app.RestartPolicy)
	if policy == "" {
		policy = "never"
	}
	maxBackoff := app.MaxRestartBackoff
	if maxBackoff <= 0 {
		maxBackoff = 5 * time.Minute
	}

	backoff := 1 * time.Second

	for {
		select {
		case <-st.stopCh:
			log.Info().Msg("supervisor stopped")
			m.mu.Lock()
			delete(m.supervisors, name)
			m.mu.Unlock()
			return
		default:
		}

		if app.Listen != 0 {
			addr := fmt.Sprintf("127.0.0.1:%d", app.Listen)
			if conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond); err == nil {
				_ = conn.Close()

				log.Error().Msgf("port %d already in use by another process", app.Listen)

				m.mu.Lock()
				delete(m.supervisors, name)
				m.mu.Unlock()
				return
			}
		}

		if len(app.Start) == 0 {
			log.Error().Msg("no start command configured")
			return
		}

		appVersion, err := m.ensureAppVersion(name)
		if err != nil {
			log.Warn().Msgf("could not determine app version before start: %v", err)
		}
		log.Debug().Msgf("starting app version %s", appVersion)

		env := CreateEnvVars([]string{}, m.user.HomeDir)
		startCmd := expandEnvSlice(app.Start, env)
		log.Debug().Msgf("starting app with command: %v", startCmd)

		cmd := exec.Command(startCmd[0], startCmd[1:]...)
		cmd.Env = env
		cmd.Dir = m.user.HomeDir

		log.Debug().Msgf("env: %v", cmd.Env)

		if !m.testMode {
			cmd.SysProcAttr = &syscall.SysProcAttr{
				Setsid: true,
				Credential: &syscall.Credential{
					Uid:    uint32(m.user.Uid),
					Gid:    uint32(m.user.Gid),
					Groups: GetSupplementalGroups(m.user.Username),
				},
			}
		}

		logFile, _, err := m.OpenLogFile(name, "app")
		if err != nil {
			log.Error().Err(err).Msg("failed to open app log file")
			return
		}
		defer logFile.Close()

		cmd.Stdout = logFile
		cmd.Stderr = logFile

		log.Info().Msg("starting app process")
		startTime := time.Now()
		if err := cmd.Start(); err != nil {
			log.Error().Err(err).Msg("failed to start app")
			if !m.shouldRestart(policy, false) {
				return
			}
			time.Sleep(backoff)
			backoff = nextBackoff(backoff, maxBackoff)
			continue
		}

		if !m.testMode {
			m.procWatcher.AddPIDIgnoreTerminate(cmd.Process.Pid)
		}

		m.mu.Lock()
		if cur, ok := m.supervisors[name]; ok {
			cur.pid = cmd.Process.Pid
		}
		m.mu.Unlock()

		doneCh := make(chan error, 1)
		go func() {
			doneCh <- cmd.Wait()
		}()

		select {
		case <-st.stopCh:
			log.Info().Msg("stop requested, killing app process")
			_ = cmd.Process.Kill()
			<-doneCh
			m.mu.Lock()
			delete(m.supervisors, name)
			m.mu.Unlock()
			return

		case err := <-doneCh:
			m.mu.Lock()
			if cur, ok := m.supervisors[name]; ok {
				cur.pid = 0
			}
			m.mu.Unlock()

			uptime := time.Since(startTime)
			if err != nil {
				log.Warn().Err(err).Dur("uptime", uptime).Msg("app exited with error")
				if !m.shouldRestart(policy, true) {
					return
				}
				log.Debug().Msgf("app will be restarted after failure, backoff=%v", backoff)
			} else {
				log.Info().Dur("uptime", uptime).Msg("app exited normally")
				if !m.shouldRestart(policy, false) {
					return
				}
				log.Debug().Msgf("app will be restarted after normal exit, backoff=%v", backoff)
			}

			m.mu.Lock()
			if cur, ok := m.supervisors[name]; ok {
				cur.restartCount++
			}
			m.mu.Unlock()

			time.Sleep(backoff)
			backoff = nextBackoff(backoff, maxBackoff)
		}
	}
}

// shouldRestart decides based on restart policy and exit condition.
func (m *AppManager) shouldRestart(policy string, failed bool) bool {
	switch policy {
	case "always":
		return true
	case "on-failure":
		return failed
	case "never":
		fallthrough
	default:
		return false
	}
}

// *** helpers

// nextBackoff calculates exponential backoff with cap.
func nextBackoff(current, max time.Duration) time.Duration {
	next := current * 2
	if next > max {
		return max
	}
	return next
}

// expandEnvWith expands ${VAR} and ${VAR:default} using a provided env slice.
func expandEnv(s string, env []string) string {
	lookup := func(key string) string {
		for _, e := range env {
			if !strings.HasPrefix(e, key+"=") {
				continue
			}
			return strings.TrimPrefix(e, key+"=")
		}
		return ""
	}

	return os.Expand(s, func(key string) string {
		parts := strings.SplitN(key, ":", 2)
		name := parts[0]
		val := lookup(name)
		if val != "" {
			return val
		}
		if len(parts) == 2 {
			return parts[1]
		}
		return ""
	})
}

func expandEnvSlice(in []string, env []string) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = expandEnv(v, env)
	}
	return out
}
