package system

import (
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
	apps       *config.Apps
	user       config.User
	stateDir   string
	logger     *zerolog.Logger
	mu         sync.Mutex
	installing map[string]bool
	testMode   bool

	supervisors map[string]*supervisorState
}

// NewAppManager creates a new AppManager instance
func NewAppManager(apps *config.Apps, user config.User, stateDir string, testMode bool) (*AppManager, error) {
	log := logger.NewLogger("app-manager")

	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}

	return &AppManager{
		apps:        apps,
		user:        user,
		stateDir:    stateDir,
		logger:      log,
		testMode:    testMode,
		installing:  make(map[string]bool),
		supervisors: make(map[string]*supervisorState),
	}, nil
}

// checkDeps verifies that all dependencies for the app are present in the system.
func (m *AppManager) checkDeps(app *config.AppSpec) error {
	for _, dep := range app.Deps {
		if _, err := exec.LookPath(dep); err != nil {
			return fmt.Errorf("missing dependency %q: %w", dep, err)
		}
	}
	return nil
}

// detectInstalled checks if the app is installed and returns its current version.
func (m *AppManager) isInstalled(ctx context.Context, app *config.AppSpec) (bool, string, error) {
	if app.Binary == "" {
		return false, "", fmt.Errorf("app binary not specified")
	}
	if _, err := os.Stat(app.Binary); err != nil {
		if os.IsNotExist(err) {
			return false, "", nil
		}
		return false, "", err
	}

	if len(app.VersionCmd) > 0 {
		cmd := exec.CommandContext(ctx, app.VersionCmd[0], app.VersionCmd[1:]...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return true, "", fmt.Errorf("version command failed: %w (output=%s)", err, string(out))
		}
		if app.VersionRegex != "" {
			re, err := regexp.Compile(app.VersionRegex)
			if err != nil {
				return true, "", fmt.Errorf("invalid versionRegex: %w", err)
			}
			matches := re.FindStringSubmatch(string(out))
			if len(matches) >= 2 {
				return true, matches[1], nil
			}
		}
		// fallback: use whole trimmed output as version
		return true, strings.TrimSpace(string(out)), nil
	}

	return true, "", nil
}

// InstallAsync starts installation in the background.
// If an install for this app is already running, it returns an error.
func (m *AppManager) InstallAsync(ctx context.Context, name string, force bool) error {
	app, ok := (*m.apps)[name]
	if !ok {
		return fmt.Errorf("app %s not found", name)
	}

	m.mu.Lock()
	if m.installing[name] {
		m.mu.Unlock()
		return fmt.Errorf("install for %s is already running", name)
	}

	if !force {
		installed, _, err := m.isInstalled(ctx, app)
		if err != nil {
			m.mu.Unlock()
			return fmt.Errorf("cannot check if %s is installed: %w", name, err)
		}
		if installed {
			m.mu.Unlock()
			return fmt.Errorf("app %s is already installed", name)
		}
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

	appStateDir := filepath.Join(m.stateDir, name)
	if err := os.MkdirAll(appStateDir, 0o755); err != nil {
		return fmt.Errorf("create app state dir: %w", err)
	}

	scriptPath := filepath.Join(appStateDir, "install.sh")
	if err := os.WriteFile(scriptPath, []byte(app.Install), 0o755); err != nil {
		return fmt.Errorf("write install script: %w", err)
	}

	outPath := filepath.Join(appStateDir, fmt.Sprintf("install-%s.out", time.Now().Format("20060102-150405")))
	outFile, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open install out log: %w", err)
	}
	defer outFile.Close()

	m.logger.Debug().Msgf("running install script for %s, output=%s, as_root=%v", name, outPath, app.InstallAsRoot)

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

	cmd.Stdout = outFile
	cmd.Stderr = outFile

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start install script: %w", err)
	}

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("install script failed: %w", err)
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
		return nil
	}

	close(st.stopCh)

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(2 * time.Second):
		return nil
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

		installed, version, err := m.isInstalled(ctx, app)
		if err != nil {
			m.logger.Warn().Msgf("detectInstalled(%s) failed: %v", name, err)
			installed = false
			version = ""
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

		if sup == nil || sup.pid == 0 {
			status.Status = "STOPPED"
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

// GetLastInstallLog returns the contents of the most recent install log.
// If no log exists, it returns empty string and no error.
func (m *AppManager) GetLastInstallLog(name string) (string, error) {
	path, err := m.GetLastInstallLogPath(name)
	if err != nil {
		return "", err
	}
	if path == "" {
		return "", nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read install log: %w", err)
	}
	return string(data), nil
}

// GetLastInstallLogPath returns the path of the most recent install log for the app.
// If no install log exists, it returns an empty string and no error.
func (m *AppManager) GetLastInstallLogPath(name string) (string, error) {
	appStateDir := filepath.Join(m.stateDir, name)

	entries, err := os.ReadDir(appStateDir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("read app state dir: %w", err)
	}

	var latestName string
	var latestTime time.Time

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if !strings.HasPrefix(n, "install-") || !strings.HasSuffix(n, ".out") {
			continue
		}

		ts := strings.TrimSuffix(strings.TrimPrefix(n, "install-"), ".out")
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

		// Optional: you can keep or remove this TCP check; it’s no longer needed for PID
		if app.Listen != 0 {
			addr := fmt.Sprintf("127.0.0.1:%d", app.Listen)
			if conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond); err == nil {
				_ = conn.Close()
				log.Debug().Msg("app already running, supervisor sleeping")
				time.Sleep(5 * time.Second)
				continue
			}
		}

		if len(app.Start) == 0 {
			log.Error().Msg("no start command configured")
			return
		}

		log.Debug().Msgf("starting app with command: %v", app.Start)

		cmd := exec.Command(app.Start[0], app.Start[1:]...)
		cmd.Env = CreateEnvVars([]string{}, m.user.HomeDir)
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

		logPath := filepath.Join(m.stateDir, fmt.Sprintf("%s.log", name))
		logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			log.Error().Err(err).Msg("open log file failed")
			return
		}

		cmd.Stdout = logFile
		cmd.Stderr = logFile

		log.Info().Msg("starting app process")
		startTime := time.Now()
		if err := cmd.Start(); err != nil {
			_ = logFile.Close()
			log.Error().Err(err).Msg("failed to start app")
			if !m.shouldRestart(policy, false) {
				return
			}
			time.Sleep(backoff)
			backoff = nextBackoff(backoff, maxBackoff)
			continue
		}
		_ = logFile.Close()

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
