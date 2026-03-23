package apps

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/k8shell-io/common/pkg/api/client/k8shelld"
	"github.com/k8shell-io/k8shelld/internal/config"
	"github.com/k8shell-io/k8shelld/internal/logger"
	"github.com/k8shell-io/k8shelld/internal/models"
	"github.com/k8shell-io/k8shelld/internal/system"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

const (
	APPS_DIR            = "/usr/local/k8shell/apps"
	VERSION_CMD_TIMEOUT = 10 * time.Second
	APP_STOP_TIMEOUT    = 5 * time.Second
)

var ErrAppNotFound = fmt.Errorf("app not found")
var ErrNoAppsConfigured = fmt.Errorf("no apps configured")
var ErrAppInvalidState = fmt.Errorf("not a valid app state")

// AppManager manages the lifecycle of applications defined in the configuration
type AppManager struct {
	apps        *config.Apps
	user        *models.User
	stateDir    string
	logger      *zerolog.Logger
	mu          sync.Mutex
	installing  map[string]bool
	testMode    bool
	procWatcher *system.ProcessWatcher
	supervisors map[string]*AppSupervisor
}

// NewAppManager creates a new AppManager instance
func NewAppManager(apps *config.Apps, user *models.User, procWatcher *system.ProcessWatcher,
	testMode bool) (*AppManager, error) {
	log := logger.NewLogger("app-manager")

	if err := os.MkdirAll(APPS_DIR, 0o755); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}

	if apps != nil {
		// The app struct does not have the Name field set from the config
		for name, app := range *apps {
			app.Name = name
		}
	}

	return &AppManager{
		apps:        apps,
		user:        user,
		procWatcher: procWatcher,
		stateDir:    APPS_DIR,
		logger:      log,
		testMode:    testMode,
		installing:  make(map[string]bool),
		supervisors: make(map[string]*AppSupervisor),
	}, nil
}

// newSupervisor creates a new AppSupervisor for the given app and adds it to the manager.
func (m *AppManager) newSupervisor(app *config.AppSpec) *AppSupervisor {
	s := NewAppSupervisor(m, app)
	m.mu.Lock()
	m.supervisors[app.Name] = s
	m.mu.Unlock()
	return s
}

// deleteSupervisor removes the supervisor for the given app from the manager.
func (m *AppManager) deleteSupervisor(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.supervisors, name)
}

// GetSupervisor returns the supervisor for the given app, if it exists.
func (m *AppManager) GetSupervisor(name string) (*AppSupervisor, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.supervisors[name]
	return s, ok
}

// ensureStateDir ensures the state directory for the app exists.
func (m *AppManager) ensureAppStateDir(name string) (string, error) {
	stateDir := filepath.Join(m.stateDir, name)
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return "", fmt.Errorf("create state dir for %s: %w", name, err)
	}
	return stateDir, nil
}

// GetApp retrieves the app specification by name.
func (m *AppManager) GetApp(name string) (*config.AppSpec, error) {
	if m.apps == nil {
		return nil, ErrNoAppsConfigured
	}
	app, ok := (*m.apps)[name]
	if !ok {
		return nil, fmt.Errorf("app %s %w", name, ErrAppNotFound)
	}
	return app, nil
}

// isAppInstalled checks if the app is installed by verifying the binary exists.
func (m *AppManager) isAppInstalled(name string) (bool, error) {
	app, err := m.GetApp(name)
	if err != nil {
		return false, err
	}

	env := system.CreateEnvVars([]string{}, m.user.GetHomeDir())
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
	app, err := m.GetApp(name)
	if err != nil {
		return "", err
	}

	if len(app.VersionCmd) == 0 || app.VersionRegex == "" {
		return "", fmt.Errorf("version command or version regex not configured for app %s", name)
	}

	env := system.CreateEnvVars([]string{}, m.user.GetHomeDir())
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
		cmd.Env = system.CreateEnvVars([]string{}, m.user.GetHomeDir())
		cmd.Dir = m.user.GetHomeDir()
		if !m.testMode {
			cmd.SysProcAttr = &syscall.SysProcAttr{
				Setsid:    true,
				Pdeathsig: 0,
				Credential: &syscall.Credential{
					Uid:    m.user.GetUID(),
					Gid:    m.user.GetGID(),
					Groups: system.GetSupplementalGroups(m.user.GetUsername()),
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
	app, err := m.GetApp(name)
	if err != nil {
		return "", err
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

// ensureAppVersion ensures the app is installed and retrieves its version.
func (m *AppManager) ensureAppVersion(name string) (string, error) {
	installed, err := m.isAppInstalled(name)
	if err != nil {
		return "", fmt.Errorf("cannot check if %s is installed: %w", name, err)
	}
	if !installed {
		return "", fmt.Errorf("%w: app %s is not installed", ErrAppInvalidState, name)
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

// parseVersion extracts the version string from the given input using the provided regex.
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
	_, err := m.GetApp(name)
	if err != nil {
		return err
	}

	if _, ok := m.supervisors[name]; ok {
		return fmt.Errorf("%w: cannot install %s while it is running", ErrAppInvalidState, name)
	}

	m.mu.Lock()
	if m.installing[name] {
		m.mu.Unlock()
		return fmt.Errorf("%w: install for %s is already running", ErrAppInvalidState, name)
	}

	if !force {
		installed, err := m.isAppInstalled(name)
		if err != nil {
			m.mu.Unlock()
			return fmt.Errorf("cannot check if %s is installed: %w", name, err)
		}
		if installed {
			m.mu.Unlock()
			return fmt.Errorf("%w: app %s is already installed", ErrAppInvalidState, name)
		}
	}
	m.mu.Unlock()

	asyncCtx := context.Background()

	go func() {
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
	app, err := m.GetApp(name)
	if err != nil {
		return err
	}

	if app.Install == "" {
		return fmt.Errorf("no install script provided for %s", app.Name)
	}

	m.mu.Lock()
	m.installing[name] = true
	m.mu.Unlock()

	defer func() {
		m.mu.Lock()
		delete(m.installing, name)
		m.mu.Unlock()
	}()

	env := system.CreateEnvVars([]string{}, m.user.GetHomeDir())
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
	defer func() {
		if err := logFile.Close(); err != nil {
			log.Error().Err(err).Msgf("failed to close install log file for %s", name)
		}
	}()

	m.logger.Debug().Msgf("running install script for %s, output=%s, as_root=%v", name, logPath, app.InstallAsRoot)

	cmd := exec.CommandContext(ctx, "/bin/sh", scriptPath)
	if app.InstallAsRoot && !m.testMode {
		cmd.Env = os.Environ()
		cmd.Dir = "/root"
	} else {
		cmd.Env = system.CreateEnvVars([]string{}, m.user.GetHomeDir())
		cmd.Dir = m.user.GetHomeDir()

		if !m.testMode {
			cmd.SysProcAttr = &syscall.SysProcAttr{
				Setsid: true,
				Credential: &syscall.Credential{
					Uid:    m.user.GetUID(),
					Gid:    m.user.GetGID(),
					Groups: system.GetSupplementalGroups(m.user.GetUsername()),
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
		m.logger.Warn().Msgf("could not write app version after install: %v", err)
	}

	return nil
}

// Start starts a supervisor for the given app if not already running.
// The supervisor will keep the app running according to its RestartPolicy.
func (m *AppManager) Start(ctx context.Context, name string) error {
	app, err := m.GetApp(name)
	if err != nil {
		return err
	}

	isInstalled, err := m.isAppInstalled(name)
	if err != nil {
		return fmt.Errorf("cannot check if %s is installed: %w", name, err)
	}
	if !isInstalled {
		return fmt.Errorf("%w: app %s is not installed", ErrAppInvalidState, name)
	}

	m.mu.Lock()
	installing := m.installing[name]
	m.mu.Unlock()
	if installing {
		return fmt.Errorf("%w: app %s is currently installing", ErrAppInvalidState, name)
	}

	_, ok := m.GetSupervisor(name)
	if ok {
		return fmt.Errorf("%w: app %s is already running", ErrAppInvalidState, name)
	}

	s := m.newSupervisor(app)
	go s.supervise()

	return nil
}

// Stop asks the supervisor to stop managing the app and kills the process if running.
func (m *AppManager) Stop(ctx context.Context, name string) error {
	_, err := m.GetApp(name)
	if err != nil {
		return err
	}

	sup, ok := m.GetSupervisor(name)
	if !ok {
		return fmt.Errorf("%w: app %s is not running", ErrAppInvalidState, name)
	}

	sup.RequestStop()

	timeout := time.After(APP_STOP_TIMEOUT)

	for {
		_, ok := m.GetSupervisor(name)
		if !ok {
			return nil
		}

		select {
		case <-timeout:
			return fmt.Errorf("timeout waiting for app %s to stop", name)
		default:
			time.Sleep(100 * time.Millisecond)
		}
	}
}

// ListAppStatus returns app status including port, PID and running time, without internal state.
func (m *AppManager) ListAppStatus(ctx context.Context) ([]k8shelld.AppStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var res []k8shelld.AppStatus
	if m.apps == nil {
		return res, nil
	}

	for name, app := range *m.apps {
		installing := m.installing[name]

		status := k8shelld.AppStatus{
			Name:     name,
			Status:   k8shelld.AppStatusUnknown,
			Version:  "",
			Port:     app.Listen,
			Protocol: app.Protocol,
			PID:      0,
			Age:      "",
			Restarts: 0,
		}

		sup, ok := m.supervisors[name]
		if ok {
			status.Restarts = sup.Restarts()
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
				v = "N/A"
			}
			version = v
		}

		status.Version = version

		if installing {
			status.Status = k8shelld.AppStatusInstalling
			res = append(res, status)
			continue
		}

		if !installed {
			status.Status = k8shelld.AppStatusNotInstalled
			res = append(res, status)
			continue
		}

		if sup == nil {
			pid, err := system.GetPIDListeningOnPort(app.Listen)
			if err != nil {
				m.logger.Warn().Msgf("Could not get PID for app %s, port %d: %v", name, app.Listen, err)
				status.Status = k8shelld.AppStatusNotStarted
				res = append(res, status)
				continue
			}

			if pid != 0 {
				status.Status = k8shelld.AppStatusInvalid
				res = append(res, status)
				continue
			}

			status.Status = k8shelld.AppStatusNotStarted
			res = append(res, status)
			continue
		}

		if sup.PID() == 0 {
			status.Status = k8shelld.AppStatusPending
			res = append(res, status)
			continue
		}

		status.PID = sup.PID()
		status.Status = k8shelld.AppStatusRunning

		if dur, err := system.GetProcessRunningTime(status.PID); err == nil {
			status.Age = formatAge(dur)
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
	return s.PID() != 0
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

// GetLastLog returns the contents of the most recent log of the given type for the app.
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

// InstallAndStart installs the app if needed and then ensures it is running.
func (m *AppManager) InstallAndStart(ctx context.Context, name string) error {
	_, err := m.GetApp(name)
	if err != nil {
		return err
	}

	installed, err := m.isAppInstalled(name)
	if err != nil {
		return fmt.Errorf("failed to check if app %s is installed: %w", name, err)
	}

	if !installed {
		m.logger.Info().Msgf("App %s not installed, installing...", name)
		if err := m.runInstall(ctx, name); err != nil {
			return fmt.Errorf("failed to install app %s: %w", name, err)
		}
	}

	if err := m.Start(ctx, name); err != nil {
		return fmt.Errorf("failed to start app %s: %w", name, err)
	}

	return nil
}

func (m *AppManager) Apps() *config.Apps {
	return m.apps
}

func formatAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}

	totalMin := int64(d / time.Minute)

	const dayMin = 24 * 60
	days := totalMin / dayMin
	hours := (totalMin % dayMin) / 60
	mins := totalMin % 60
	secs := int64(d/time.Second) % 60

	if days > 0 {
		if mins == 0 {
			return fmt.Sprintf("%dd%dh", days, hours)
		}
		return fmt.Sprintf("%dd%dh", days, hours)
	}

	if hours > 0 {
		if mins == 0 {
			return fmt.Sprintf("%dh", hours)
		}
		return fmt.Sprintf("%dh%dm", hours, mins)
	}

	if mins > 0 {
		if secs == 0 {
			return fmt.Sprintf("%dm", mins)
		}
		return fmt.Sprintf("%dm%ds", mins, secs)
	}

	return fmt.Sprintf("%ds", secs)
}
