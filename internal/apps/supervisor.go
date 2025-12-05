package apps

import (
	"fmt"
	"net"
	"os/exec"
	"strings"
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
	manager      *AppManager
	app          *config.AppSpec
	log          zerolog.Logger
	stopCh       chan struct{}
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
			if conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond); err == nil {
				_ = conn.Close()

				s.log.Error().Msgf("port %d already in use by another process", s.app.Listen)
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

		env := system.CreateEnvVars([]string{}, s.manager.user.HomeDir)
		startCmd := expandEnvSlice(s.app.Start, env)
		s.log.Debug().Msgf("starting app with command: %v", startCmd)
		cmd := exec.Command(startCmd[0], startCmd[1:]...)
		cmd.Env = env
		cmd.Dir = s.manager.user.HomeDir

		s.log.Debug().Msgf("env: %v", cmd.Env)

		if !s.manager.testMode {
			cmd.SysProcAttr = &syscall.SysProcAttr{
				Setsid: true,
				Credential: &syscall.Credential{
					Uid:    uint32(s.manager.user.Uid),
					Gid:    uint32(s.manager.user.Gid),
					Groups: system.GetSupplementalGroups(s.manager.user.Username),
				},
			}
		}

		logFile, _, err := s.manager.OpenLogFile(s.app.Name, "app")
		if err != nil {
			s.log.Error().Err(err).Msg("failed to open app log file")
			return
		}
		defer logFile.Close()

		cmd.Stdout = logFile
		cmd.Stderr = logFile

		log.Info().Msg("starting app process")
		startTime := time.Now()
		if err := cmd.Start(); err != nil {
			log.Error().Err(err).Msg("failed to start app")
			if !s.manager.shouldRestart(policy, false) {
				return
			}
			time.Sleep(backoff)
			backoff = nextBackoff(backoff, maxBackoff)
			continue
		}

		if !s.manager.testMode {
			s.manager.procWatcher.AddPIDIgnoreTerminate(cmd.Process.Pid)
		}

		s.pid = cmd.Process.Pid
		log.Info().Msgf("app process started with PID %d", s.pid)

		doneCh := make(chan error, 1)
		go func() {
			doneCh <- cmd.Wait()
		}()

		select {
		case <-s.stopCh:
			log.Info().Msg("stop requested, killing app process")
			_ = cmd.Process.Kill()
			<-doneCh
			s.manager.deleteSupervisor(s.app.Name)
			return

		case err := <-doneCh:
			s.pid = 0

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

			s.restartCount++
			time.Sleep(backoff)
			backoff = nextBackoff(backoff, maxBackoff)
		}
	}
}
