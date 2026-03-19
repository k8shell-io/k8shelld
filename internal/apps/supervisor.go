package apps

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/k8shell-io/k8shelld/internal/config"
	"github.com/k8shell-io/k8shelld/internal/logger"
	"github.com/k8shell-io/k8shelld/internal/system"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// supervisorState holds the state for a supervisor
type AppSupervisor struct {
	manager *AppManager
	app     *config.AppSpec
	log     zerolog.Logger

	stopCh   chan struct{}
	stopOnce sync.Once

	mu           sync.Mutex
	restartCount int
	pid          int
}

func NewAppSupervisor(manager *AppManager, app *config.AppSpec) *AppSupervisor {
	log := logger.NewLogger("app-supervisor").With().Str("app", app.Name).Logger()
	return &AppSupervisor{
		manager: manager,
		app:     app,
		log:     log,
		stopCh:  make(chan struct{}),
	}
}

func (s *AppSupervisor) RequestStop() {
	s.stopOnce.Do(func() { close(s.stopCh) })
}

func (s *AppSupervisor) PID() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pid
}

func (s *AppSupervisor) setPID(pid int) {
	s.mu.Lock()
	s.pid = pid
	s.mu.Unlock()
}

func (s *AppSupervisor) Restarts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.restartCount
}

func (s *AppSupervisor) incRestarts() {
	s.mu.Lock()
	s.restartCount++
	s.mu.Unlock()
}

// supervise runs the supervisor loop for the app.
func (s *AppSupervisor) supervise() {
	policy := strings.ToLower(s.app.RestartPolicy)
	if policy == "" {
		policy = "never"
	}
	maxBackoff := s.app.MaxRestartBackoff
	if maxBackoff <= 0 {
		maxBackoff = 5 * time.Minute
	}

	backoff := 1 * time.Second

	for {
		select {
		case <-s.stopCh:
			s.log.Info().Msg("supervisor stopped")
			s.manager.deleteSupervisor(s.app.Name)
			return
		default:
		}

		if s.app.Listen != 0 {
			addr := fmt.Sprintf("127.0.0.1:%d", s.app.Listen)
			if err := waitForTCPBindable(addr, 5*time.Second); err != nil {
				s.log.Error().Msgf("port %d still in use, cannot start: %v", s.app.Listen, err)
				s.manager.deleteSupervisor(s.app.Name)
				return
			}
		}

		if len(s.app.Start) == 0 {
			s.log.Error().Msg("no start command configured")
			return
		}

		appVersion, err := s.manager.ensureAppVersion(s.app.Name)
		if err != nil {
			s.log.Warn().Msgf("could not determine app version before start: %v", err)
		}
		s.log.Debug().Msgf("starting app version %s", appVersion)

		env := system.CreateEnvVars([]string{}, s.manager.user.GetHomeDir())
		startCmd := expandEnvSlice(s.app.Start, env)
		s.log.Debug().Msgf("starting app with command: %v", startCmd)
		cmd := exec.Command(startCmd[0], startCmd[1:]...)
		cmd.Env = env
		cmd.Dir = s.manager.user.GetHomeDir()

		s.log.Debug().Msgf("env: %v", cmd.Env)

		if !s.manager.testMode {
			cmd.SysProcAttr = &syscall.SysProcAttr{
				Setsid: true,
				Credential: &syscall.Credential{
					Uid:    s.manager.user.GetUID(),
					Gid:    s.manager.user.GetGID(),
					Groups: system.GetSupplementalGroups(s.manager.user.GetUsername()),
				},
			}
		}

		logFile, _, err := s.manager.OpenLogFile(s.app.Name, "app")
		if err != nil {
			s.log.Error().Err(err).Msg("failed to open app log file")
			return
		}
		defer func() {
			if err := logFile.Close(); err != nil {
				s.log.Error().Msg("failed to close app log file")
			}
		}()

		cmd.Stdout = logFile
		cmd.Stderr = logFile

		s.log.Info().Msg("starting app process")
		startTime := time.Now()
		if err := cmd.Start(); err != nil {
			s.log.Error().Err(err).Msg("failed to start app")

			if !s.manager.shouldRestart(policy, true) {
				return
			}
			time.Sleep(backoff)
			backoff = nextBackoff(backoff, maxBackoff)
			continue
		}

		if !s.manager.testMode {
			s.manager.procWatcher.AddPIDIgnoreTerminate(cmd.Process.Pid)
		}

		s.setPID(cmd.Process.Pid)
		s.log.Info().Msgf("app process started with PID %d", s.PID())

		doneCh := make(chan error, 1)
		go func() { doneCh <- cmd.Wait() }()

		select {
		case <-s.stopCh:
			s.log.Info().Msg("stop requested, terminating app process group")
			s.killApp(cmd, doneCh, 3*time.Second)
			s.manager.deleteSupervisor(s.app.Name)
			return

		case err := <-doneCh:
			s.setPID(0)

			uptime := time.Since(startTime)
			if err != nil {
				log.Warn().Err(err).Dur("uptime", uptime).Msg("app exited with error")
				if !s.manager.shouldRestart(policy, true) {
					return
				}
				log.Debug().Msgf("app will be restarted after failure, backoff=%v", backoff)
			} else {
				log.Info().Dur("uptime", uptime).Msg("app exited normally")
				if !s.manager.shouldRestart(policy, false) {
					return
				}
				log.Debug().Msgf("app will be restarted after normal exit, backoff=%v", backoff)
			}

			s.incRestarts()
			time.Sleep(backoff)
			backoff = nextBackoff(backoff, maxBackoff)
		}
	}
}

// killApp attempts to gracefully terminate the app process group,
// then forcefully kills it if it doesn't exit within the grace period.
func (s *AppSupervisor) killApp(cmd *exec.Cmd, doneCh <-chan error, grace time.Duration) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid

	if !s.manager.testMode {
		_ = syscall.Kill(-pid, syscall.SIGTERM)
	} else {
		_ = cmd.Process.Signal(os.Interrupt)
	}

	select {
	case <-time.After(grace):
		if !s.manager.testMode {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		} else {
			_ = cmd.Process.Kill()
		}
	case <-doneCh:
		return
	}

	select {
	case <-time.After(5 * time.Second):
	case <-doneCh:
	}
}

func waitForTCPBindable(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			_ = ln.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
}
