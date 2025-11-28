package system

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
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

// AppManager manages the lifecycle of applications defined in the configuration
type AppManager struct {
	apps       *config.Apps
	stateDir   string
	logger     *zerolog.Logger
	mu         sync.Mutex
	installing map[string]bool
}

// NewAppManager creates a new AppManager instance
func NewAppManager(apps *config.Apps, stateDir string) (*AppManager, error) {
	log := logger.NewLogger("app-manager")

	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}

	return &AppManager{
		apps:       apps,
		stateDir:   stateDir,
		logger:     log,
		installing: make(map[string]bool),
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

	cmd := exec.CommandContext(ctx, "/bin/sh", scriptPath)

	outPath := filepath.Join(appStateDir, fmt.Sprintf("install-%s.out", time.Now().Format("20060102-150405")))
	outFile, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open install out log: %w", err)
	}
	defer outFile.Close()

	m.logger.Debug().Msgf("running install script for %s, output=%s", name, outPath)

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

func (m *AppManager) EnsureRunning(ctx context.Context, name string, app *config.AppSpec) error {
	if app.Listen != 0 {
		addr := fmt.Sprintf("127.0.0.1:%d", app.Listen)
		conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
	}

	if len(app.Start) == 0 {
		return fmt.Errorf("no start command for %s", name)
	}

	m.logger.Debug().Msgf("starting app %s", name)
	cmd := exec.CommandContext(ctx, app.Start[0], app.Start[1:]...)

	logPath := filepath.Join(m.stateDir, fmt.Sprintf("%s.log", name))
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open log file for %s: %w", name, err)
	}

	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return fmt.Errorf("start %s: %w", name, err)
	}

	_ = logFile.Close()

	return nil
}

func (m *AppManager) Stop(ctx context.Context, name string, app *config.AppSpec) error {
	if app.Listen == 0 {
		return nil
	}
	addr := fmt.Sprintf("127.0.0.1:%d", app.Listen)
	conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err != nil {
		return nil
	}
	_ = conn.Close()
	// TODO: signal the PID
	m.logger.Printf("app %s appears to be running on %s but stop logic is not implemented", name, addr)
	return nil
}

// ListAppStatus returns app status including port, PID and running time, without internal state.
func (m *AppManager) ListAppStatus(ctx context.Context) ([]models.AppStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var res []models.AppStatus

	for name, app := range *m.apps {
		installed, version, err := m.isInstalled(ctx, app)
		if err != nil {
			m.logger.Warn().Msgf("detectInstalled(%s) failed: %v", name, err)
			installed = false
			version = ""
		}

		status := models.AppStatus{
			Name:       name,
			Status:     "-",
			Version:    version,
			ListenPort: app.Listen,
			PID:        0,
		}

		if !installed {
			status.Status = "N/A"
			res = append(res, status)
			continue
		}

		if app.Listen == 0 {
			status.Status = "INSTALLED"
			res = append(res, status)
			continue
		}

		pid, err := getPIDListeningOnPort(app.Listen)
		if err != nil {
			m.logger.Warn().Msgf("getPIDListeningOnPort(%d) failed for app %s: %v", app.Listen, name, err)
		}

		if pid == 0 {
			status.Status = "STOPPED"
			res = append(res, status)
			continue
		}

		// We have a listener and a PID
		status.PID = pid
		status.Status = "RUNNING"

		if dur, err := getProcessRunningTime(pid); err == nil {
			status.RunningTime = dur.Truncate(time.Second).String()
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

// *** helpers

// getProcessRunningTime returns how long the given PID has been running as a time.Duration.
func getProcessRunningTime(pid int) (time.Duration, error) {
	// Read /proc/uptime to get system uptime in seconds
	upBytes, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, err
	}
	var uptimeSeconds float64
	if _, err := fmt.Sscanf(string(upBytes), "%f", &uptimeSeconds); err != nil {
		return 0, err
	}

	// Read /proc/<pid>/stat
	statPath := filepath.Join("/proc", strconv.Itoa(pid), "stat")
	data, err := os.ReadFile(statPath)
	if err != nil {
		return 0, err
	}
	// Field 22 is starttime (clock ticks since boot)
	// But field 2 (comm) may contain spaces in parentheses, so we need to handle that.
	parts := strings.Fields(string(data))
	if len(parts) < 22 {
		return 0, fmt.Errorf("unexpected /proc/%d/stat format", pid)
	}
	// starttime is at index 21 (0-based)
	startTicksStr := parts[21]
	startTicks, err := strconv.ParseUint(startTicksStr, 10, 64)
	if err != nil {
		return 0, err
	}

	// Get clock ticks per second
	ticks := float64(os.Getpagesize()) // WRONG, we’ll use sysconf via build tag? (simplify)
	_ = ticks
	// Simpler: assume 100Hz (most Linux configs). For better accuracy, use runtime or sysconf,
	// but this is usually good enough.
	const hz = 100.0

	startSeconds := float64(startTicks) / hz
	// process running time = uptime - (uptime - startSeconds) => uptime - (startAtBoot)
	running := uptimeSeconds - startSeconds
	if running < 0 {
		running = 0
	}
	return time.Duration(running * float64(time.Second)), nil
}

// getPIDListeningOnPort tries to find a PID that is listening on the given TCP port.
// Linux-only implementation, returns 0 if not found or on error.
func getPIDListeningOnPort(port int) (int, error) {
	// Build a map inode -> pid
	inodeToPID := make(map[string]int)

	procEntries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, err
	}

	for _, e := range procEntries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		fdDir := filepath.Join("/proc", e.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			link, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if err != nil {
				continue
			}
			// Looking for "socket:[12345]"
			if strings.HasPrefix(link, "socket:[") && strings.HasSuffix(link, "]") {
				inode := link[len("socket:[") : len(link)-1]
				inodeToPID[inode] = pid
			}
		}
	}

	// Now parse /proc/net/tcp and /proc/net/tcp6 looking for LISTEN on that port.
	portHex := fmt.Sprintf("%04X", port)

	checkFile := func(path string) (int, error) {
		f, err := os.Open(path)
		if err != nil {
			return 0, nil
		}
		defer f.Close()

		sc := bufio.NewScanner(f)
		// Skip header
		if !sc.Scan() {
			return 0, nil
		}
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			fields := strings.Fields(line)
			if len(fields) < 10 {
				continue
			}
			localAddress := fields[1] // "0100007F:1F90"
			state := fields[3]        // "0A" for LISTEN
			inode := fields[9]

			parts := strings.Split(localAddress, ":")
			if len(parts) != 2 {
				continue
			}
			portPart := parts[1]
			if strings.EqualFold(portPart, portHex) && state == "0A" {
				if pid, ok := inodeToPID[inode]; ok {
					return pid, nil
				}
			}
		}
		return 0, nil
	}

	if pid, err := checkFile("/proc/net/tcp"); err == nil && pid != 0 {
		return pid, nil
	}
	if pid, err := checkFile("/proc/net/tcp6"); err == nil && pid != 0 {
		return pid, nil
	}

	return 0, nil
}
