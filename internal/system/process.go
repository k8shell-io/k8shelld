package system

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/k8shell-io/k8shelld/internal/log"
	"github.com/rs/zerolog"
)

// Map of signal names to their corresponding syscall.Signal values
var SignalMap = map[string]syscall.Signal{
	"ABRT": syscall.SIGABRT,
	"ALRM": syscall.SIGALRM,
	"FPE":  syscall.SIGFPE,
	"HUP":  syscall.SIGHUP,
	"ILL":  syscall.SIGILL,
	"INT":  syscall.SIGINT,
	"KILL": syscall.SIGKILL,
	"PIPE": syscall.SIGPIPE,
	"QUIT": syscall.SIGQUIT,
	"SEGV": syscall.SIGSEGV,
	"TERM": syscall.SIGTERM,
	"USR1": syscall.SIGUSR1,
	"USR2": syscall.SIGUSR2,
	"CHLD": syscall.SIGCHLD,
	"CONT": syscall.SIGCONT,
	"STOP": syscall.SIGSTOP,
	"TSTP": syscall.SIGTSTP,
	"TTIN": syscall.SIGTTIN,
	"TTOU": syscall.SIGTTOU,
	"URG":  syscall.SIGURG,
	"XCPU": syscall.SIGXCPU,
	"XFSZ": syscall.SIGXFSZ,
}

// Function to convert signal name to syscall.Signal value
func GetSignalValue(name string) (syscall.Signal, error) {
	signal, ok := SignalMap[name]
	if !ok {
		return -1, fmt.Errorf("unknown signal: %s", name)
	}
	return signal, nil
}

type ProcessWatcher struct {
	logger              *zerolog.Logger
	zombies             bool
	orphans             bool
	checkInterval       int
	excludePatterns     []*regexp.Regexp
	ignoreSIGHUPTable   map[int]bool
	ignorePIDsMutex     sync.Mutex
	ignorePIDsTerminate []int
}

type ProcessInfo struct {
	ppid         int
	ignoreSIGHUP bool
	cmdline      string
}

func NewProcessWatcher(terminateOrphans bool, reapZombies bool, checkInterval int, excludePatterns []string) *ProcessWatcher {
	p := &ProcessWatcher{
		logger:              log.NewLogger("process-watcher"),
		ignoreSIGHUPTable:   make(map[int]bool),
		orphans:             terminateOrphans,
		zombies:             reapZombies,
		checkInterval:       checkInterval,
		ignorePIDsMutex:     sync.Mutex{},
		ignorePIDsTerminate: []int{},
	}

	// Compile exclude patterns
	for _, exclude := range excludePatterns {
		pattern, err := regexp.Compile(exclude)
		if err != nil {
			p.logger.Warn().Msgf("Invalid exclude pattern '%s': %v", exclude, err)
		}
		p.excludePatterns = append(p.excludePatterns, pattern)
	}
	return p
}

func (p *ProcessWatcher) AddPIDIgnoreTerminate(pid int) {
	p.ignorePIDsMutex.Lock()
	defer p.ignorePIDsMutex.Unlock()
	p.ignorePIDsTerminate = append(p.ignorePIDsTerminate, pid)
}

func (p *ProcessWatcher) Run(ctx context.Context) {
	// Channel to receive OS signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGCHLD)

	// Reaper for orphaned processes (if enabled)
	if p.orphans {
		go func() {
			ticker := time.NewTicker(time.Duration(p.checkInterval) * time.Second)
			defer ticker.Stop()

			for {
				select {
				case <-ctx.Done():
					p.logger.Info().Msgf("ProcessWatcher: Stopping orphan termination loop")
					return
				case <-ticker.C:
					p.terminateOrphans()
				}
			}
		}()
	}

	// Reaper for zombie processes on SIGCHLD (if enabled)
	for {
		select {
		case <-ctx.Done():
			p.logger.Info().Msgf("ProcessWatcher: Context cancelled, stopping signal handler")
			return
		case sig := <-sigChan:
			switch sig {
			case syscall.SIGCHLD:
				if p.zombies {
					p.reapZombies()
				}
			}
		}
	}
}

func (p *ProcessWatcher) getProcessInfo(pid int) (ProcessInfo, error) {
	var info = ProcessInfo{
		ppid:         -1,
		ignoreSIGHUP: false,
		cmdline:      "",
	}

	statusPath := fmt.Sprintf("/proc/%d/status", pid)
	data, err := os.ReadFile(statusPath)
	if err != nil {
		return info, fmt.Errorf("error reading %s: %v", statusPath, err)
	}

	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		// get PPID field
		if strings.HasPrefix(line, "PPid:") {
			ppidStr := strings.TrimSpace(strings.TrimPrefix(line, "PPid:"))
			info.ppid, err = strconv.Atoi(ppidStr)
			if err != nil {
				return info, fmt.Errorf("error parsing PPid: %v", err)
			}
		}

		// Check for SigIgn field
		if strings.HasPrefix(line, "SigIgn:") {
			sigIgn := strings.TrimSpace(strings.TrimPrefix(line, "SigIgn:"))
			value, err := strconv.ParseUint(sigIgn, 16, 64) // Parse hexadecimal
			if err != nil {
				return info, fmt.Errorf("error parsing SigIgn: %v", err)
			}
			info.ignoreSIGHUP = value&1 != 0
		}

		// Break early if both fields are found
		if info.ppid != -1 && info.ignoreSIGHUP {
			break
		}
	}

	cmdlineFile := fmt.Sprintf("/proc/%d/cmdline", pid)
	cmdline, err := os.ReadFile(cmdlineFile)
	if err != nil {
		return info, fmt.Errorf("error reading PID %s cmdline: %v", cmdlineFile, err)
	}
	info.cmdline = strings.Join(strings.Split(string(cmdline), "\x00"), " ")

	return info, nil
}

func (p *ProcessWatcher) reapZombies() {
	for {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
		if pid <= 0 || err != nil {
			break
		}
		p.logger.Info().Msgf("Reaped zombie process PID %d", pid)
	}
}

func (p *ProcessWatcher) terminateOrphans() {
	validPIDs := make(map[int]bool)
	files, err := os.ReadDir("/proc")
	if err != nil {
		p.logger.Error().Msgf("Failed to read /proc: %v", err)
		return
	}

	for _, file := range files {
		pid, err := strconv.Atoi(file.Name())
		if err != nil {
			continue
		}

		validPIDs[pid] = true

		if p.ignoreSIGHUPTable[pid] {
			continue
		}

		p.ignorePIDsMutex.Lock()
		found := false
		for _, ignorePID := range p.ignorePIDsTerminate {
			if pid == ignorePID {
				p.logger.Debug().Msgf("Ignoring PID %d, in ignore list", pid)
				p.ignoreSIGHUPTable[pid] = true
				found = true
				break
			}
		}
		p.ignorePIDsMutex.Unlock()
		if found {
			continue
		}

		info, err := p.getProcessInfo(pid)
		if err != nil {
			p.logger.Error().Msgf("Failed to get process info for PID %d: %v", pid, err)
			continue
		}

		if info.ppid == 1 {
			// Check if the process group leader has the SIGHUP signal ignored
			if info.ignoreSIGHUP {
				p.ignoreSIGHUPTable[pid] = true
				p.logger.Debug().Msgf("Ignoring PID %d, SigIgn set", pid)
				continue
			}

			// Check if the cmdline matches any of the exclude patterns
			found := false
			for _, pattern := range p.excludePatterns {
				if pattern.MatchString(info.cmdline) {
					p.logger.Info().Msgf("Excluding orphaned PID %d, cmdline: %s, pattern: %s", pid, info.cmdline, pattern.String())
					p.ignoreSIGHUPTable[pid] = true
					found = true
					break
				}
			}
			if found {
				continue
			}

			// Terminate the process group
			err = syscall.Kill(-pid, syscall.SIGHUP)
			if err == nil {
				p.logger.Info().Msgf("Sent SIGHUP to orphaned process group PID %d", pid)
			} else {
				err = syscall.Kill(pid, syscall.SIGHUP)
				if err != nil {
					p.logger.Error().Msgf("Failed to send SIGHUP to PID %d: %v, sending SIGKILL...", pid, err)
					err = syscall.Kill(pid, syscall.SIGKILL)
					if err != nil {
						p.logger.Error().Msgf("Failed to send SIGKILL to PID %d: %v", pid, err)
					}
				}
			}
		}
	}

	// Clean up stale entries in ignoreSIGHUPTable
	for pid := range p.ignoreSIGHUPTable {
		if !validPIDs[pid] {
			delete(p.ignoreSIGHUPTable, pid)
		}
	}

	p.ignorePIDsMutex.Lock()
	newIgnoreList := []int{}
	for pid := range p.ignorePIDsTerminate {
		if validPIDs[pid] {
			newIgnoreList = append(newIgnoreList, pid)
		}
	}
	p.ignorePIDsTerminate = newIgnoreList
	p.ignorePIDsMutex.Unlock()
}
