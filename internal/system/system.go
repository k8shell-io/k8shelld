package system

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Paths to cgroups v2 files
const (
	cpuStatPath    = "/sys/fs/cgroup/cpu.stat"
	cpuMaxPath     = "/sys/fs/cgroup/cpu.max"
	memCurrentPath = "/sys/fs/cgroup/memory.current"
	memMaxPath     = "/sys/fs/cgroup/memory.max"
)

const MaxCPUSamples = 100 // Maximum number of CPU samples to keep in history

// Usage struct to hold CPU & Memory metrics
// The metrics are collected from cgroups v2 files
type SystemInfo struct {
	CollectedAt        time.Time    // Time of collection
	CPUUsageUsec       int64        // CPU usage in microseconds
	CPUUsageMillicores float64      // CPU usage in mCPU
	MemoryUsageMiB     float64      // Memory usage in MiB
	CPULimitMillicores float64      // CPU limit in mCPU (if set)
	MemLimitMiB        float64      // Memory limit in MiB (if set)
	CPUUsageSeconds    float64      // CPU usage in seconds
	CPUAvg1Min         float64      // CPU usage average over 1 minute
	CPUAvg5Min         float64      // CPU usage average over 5 minutes
	CPUAvg15Min        float64      // CPU usage average over 15 minutes
	stats              *SystemStats // System statistics
}

// CPUSample represents a CPU usage sample
type CPUSample struct {
	Timestamp time.Time
	Usage     float64
}

// SystemStats holds the history of CPU usage samples
type SystemStats struct {
	CPUHistory []CPUSample
	MaxSamples int
}

// NewSystemStats initializes a new SystemStats instance
func (s *SystemStats) AddSample(sample CPUSample) {
	s.CPUHistory = append(s.CPUHistory, sample)
	if len(s.CPUHistory) > s.MaxSamples {
		s.CPUHistory = s.CPUHistory[len(s.CPUHistory)-s.MaxSamples:]
	}
}

// GetAverage returns the average CPU usage in millicores over the specified duration
func (s *SystemStats) GetAverage(duration time.Duration) float64 {
	cutoff := time.Now().Add(-duration)
	var total float64
	var count int64
	for _, sample := range s.CPUHistory {
		if sample.Timestamp.After(cutoff) {
			total += sample.Usage
			count++
		}
	}
	if count == 0 {
		return 0
	}
	return float64(total) / float64(count)
}

// Utility functions

// Read an integer value from a file, given a field name
func keyReadIntFromFile(path, field string) (int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, field) {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			return strconv.ParseInt(fields[1], 10, 64)
		}
	}
	return 0, fmt.Errorf("%s not found in %s", field, path)
}

// Read an integer value from a file
func readIntFromFile(path string) (int64, error) {
	str, err := readStringFromFile(path)
	if err != nil {
		return -1, err
	}
	return strconv.ParseInt(str, 10, 64)
}

// Read a string from a file
func readStringFromFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// Uptime

// getStartTimeFromProcStat retrieves the start time of the process from /proc/1/stat
func GetStartTimeFromProcStat() (time.Time, error) {
	data, err := os.ReadFile("/proc/1/stat")
	if err != nil {
		return time.Time{}, err
	}

	fields := strings.Fields(string(data))
	if len(fields) < 22 {
		return time.Time{}, fmt.Errorf("unexpected stat format")
	}

	startTimeTicks, err := strconv.ParseInt(fields[21], 10, 64)
	if err != nil {
		return time.Time{}, err
	}

	// Assume 100 clock ticks/sec
	const clockTicksPerSecond = 100
	startSeconds := float64(startTimeTicks) / float64(clockTicksPerSecond)

	uptimeBytes, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return time.Time{}, err
	}

	uptimeStr := strings.Fields(string(uptimeBytes))[0]
	uptimeSeconds, err := strconv.ParseFloat(uptimeStr, 64)
	if err != nil {
		return time.Time{}, err
	}

	now := time.Now()
	bootTime := now.Add(-time.Duration(uptimeSeconds) * time.Second)
	startTime := bootTime.Add(time.Duration(startSeconds) * time.Second)

	return startTime, nil
}

// CPU and Memory Usage and Limits

// getCPUUsage retrieves CPU usage in millicores and seconds
func getCPUUsage(previousUsage int64, prevTime time.Time) (float64, float64, int64, time.Time, error) {
	usageUsec, err := keyReadIntFromFile(cpuStatPath, "usage_usec")
	if err != nil {
		return 0, 0, 0, time.Now(), err
	}

	cpuUsageSeconds := float64(usageUsec) / (100 * 1e6)

	now := time.Now()
	elapsedTime := now.Sub(prevTime).Seconds()
	if previousUsage > 0 && elapsedTime > 0 {
		usageDelta := usageUsec - previousUsage
		cpuUsage := (float64(usageDelta) / (elapsedTime * 1000))
		return cpuUsage, cpuUsageSeconds, usageUsec, now, nil
	}

	return 0, cpuUsageSeconds, usageUsec, now, nil
}

// getCPULimits retrieves CPU limits (if defined)
func getCPULimits() (float64, error) {
	data, err := readStringFromFile(cpuMaxPath)
	if err != nil {
		return 0, err
	}
	parts := strings.Fields(data)
	if parts[0] == "max" {
		return -1, nil // No limit
	}
	quota, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, err
	}
	period, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, err
	}

	return (float64(quota) / float64(period)) * 1000, nil
}

// getMemoryUsage retrieves memory usage in MiB
func getMemoryUsage() (float64, error) {
	memBytes, err := readIntFromFile(memCurrentPath)
	if err != nil {
		return 0, err
	}
	return float64(memBytes) / (1024 * 1024), nil
}

// getMemoryLimit retrieves memory limits (if defined)
func getMemoryLimit() (float64, error) {
	memLimitStr, err := readStringFromFile(memMaxPath)
	if err != nil {
		return 0, err
	}
	if memLimitStr == "max" {
		return -1, nil // No limit
	}
	memBytes, err := strconv.ParseInt(memLimitStr, 10, 64)
	if err != nil {
		return 0, err
	}
	return float64(memBytes) / (1024 * 1024), nil
}

// Retrieve all usage metrics
func UpdateSystemInfo(systemInfo *SystemInfo) (*SystemInfo, error) {
	var prevUsage int64 = 0
	var prevTime time.Time = time.Now()

	if systemInfo != nil {
		prevUsage = systemInfo.CPUUsageUsec
		prevTime = systemInfo.CollectedAt
	} else {
		systemInfo = &SystemInfo{stats: &SystemStats{MaxSamples: MaxCPUSamples}}
	}

	cpuUsage, cpuUsageSeconds, newUsage, newTime, err := getCPUUsage(prevUsage, prevTime)
	if err != nil {
		return nil, err
	}

	cpuLimit, err := getCPULimits()
	if err != nil {
		return nil, err
	}

	memUsage, err := getMemoryUsage()
	if err != nil {
		return nil, err
	}

	memLimit, err := getMemoryLimit()
	if err != nil {
		return nil, err
	}

	systemInfo.CPUUsageUsec = newUsage
	systemInfo.CPUUsageMillicores = cpuUsage
	systemInfo.CPULimitMillicores = cpuLimit
	systemInfo.MemoryUsageMiB = memUsage
	systemInfo.MemLimitMiB = memLimit
	systemInfo.CPUUsageSeconds = cpuUsageSeconds
	systemInfo.CollectedAt = newTime

	// Add CPU sample and calculate averages
	if cpuLimit > 0 {
		systemInfo.stats.AddSample(CPUSample{Timestamp: newTime, Usage: (cpuUsage / cpuLimit) * 100})
	} else {
		systemInfo.stats.AddSample(CPUSample{Timestamp: newTime, Usage: 0})
	}

	systemInfo.CPUAvg1Min = systemInfo.stats.GetAverage(1 * time.Minute)
	systemInfo.CPUAvg5Min = systemInfo.stats.GetAverage(5 * time.Minute)
	systemInfo.CPUAvg15Min = systemInfo.stats.GetAverage(15 * time.Minute)

	return systemInfo, nil
}

// CreateEnvVars creates new environment variables with the provided environment variables and a home directory.
// It includes all existing environment variables and ensures that duplicate keys are not added and that
// the HOME variable is set correctly.
func CreateEnvVars(envVars []string, homeDir string) []string {
	newEnv := []string{}
	addedKeys := make(map[string]struct{})

	extractKey := func(env string) string {
		if i := strings.Index(env, "="); i >= 0 {
			return env[:i]
		}
		return env
	}

	for _, e := range envVars {
		key := extractKey(e)
		if _, exists := addedKeys[key]; !exists {
			newEnv = append(newEnv, e)
			addedKeys[key] = struct{}{}
		}
	}

	newEnv = append(newEnv, fmt.Sprintf("HOME=%s", homeDir))
	addedKeys["HOME"] = struct{}{}

	for _, e := range os.Environ() {
		key := extractKey(e)
		if key == "HOME" {
			continue
		}
		if _, exists := addedKeys[key]; !exists {
			newEnv = append(newEnv, e)
			addedKeys[key] = struct{}{}
		}
	}
	return newEnv
}

// GetProcessRunningTime returns how long the given PID has been running as a time.Duration.
func GetProcessRunningTime(pid int) (time.Duration, error) {
	upBytes, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, err
	}
	var uptimeSeconds float64
	if _, err := fmt.Sscanf(string(upBytes), "%f", &uptimeSeconds); err != nil {
		return 0, err
	}

	statPath := filepath.Join("/proc", strconv.Itoa(pid), "stat")
	data, err := os.ReadFile(statPath)
	if err != nil {
		return 0, err
	}
	// Field 22 is starttime (clock ticks since boot)
	// field 2 (comm) may contain spaces in parentheses
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

	hz := 100.0
	startSeconds := float64(startTicks) / hz
	running := uptimeSeconds - startSeconds
	if running < 0 {
		running = 0
	}
	return time.Duration(running * float64(time.Second)), nil
}

// GetPIDListeningOnPort tries to find a PID that is listening on the given TCP port.
func GetPIDListeningOnPort(port int) (int, error) {
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
			if strings.HasPrefix(link, "socket:[") && strings.HasSuffix(link, "]") {
				inode := link[len("socket:[") : len(link)-1]
				inodeToPID[inode] = pid
			}
		}
	}

	portHex := fmt.Sprintf("%04X", port)

	checkFile := func(path string) (int, error) {
		f, err := os.Open(path)
		if err != nil {
			return 0, nil
		}
		defer f.Close()

		sc := bufio.NewScanner(f)
		if !sc.Scan() {
			return 0, nil
		}
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			fields := strings.Fields(line)
			if len(fields) < 10 {
				continue
			}
			localAddress := fields[1]
			state := fields[3]
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
