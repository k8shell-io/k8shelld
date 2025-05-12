package server

import (
	"fmt"
	"os"
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

// Usage struct to hold CPU & Memory metrics
// The metrics are collected from cgroups v2 files
type SystemInfo struct {
	CPUUsageUsec       int64     // CPU usage in microseconds
	CPUUsageMillicores float64   // CPU usage in mCPU
	MemoryUsageMiB     float64   // Memory usage in MiB
	CPULimitMillicores float64   // CPU limit in mCPU (if set)
	MemLimitMiB        float64   // Memory limit in MiB (if set)
	CPUUsageSeconds    float64   // CPU usage in seconds
	CollectedAt        time.Time // Time of collection
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

// Convert CPU usage seconds to `MM:SS` format (like `ps` command)
func formatCPUTime(seconds float64) string {
	minutes := int(seconds) / 60
	secs := int(seconds) % 60
	return fmt.Sprintf("%02d:%02d", minutes, secs)
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
		systemInfo = &SystemInfo{}
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

	return systemInfo, nil

}
