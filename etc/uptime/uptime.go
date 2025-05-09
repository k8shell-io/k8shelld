package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

func getStartTimeFromProcStat() (time.Time, error) {
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

	// Most systems use 100 clock ticks/sec
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

func formatPrettyTime(d time.Duration) string {
	seconds := int64(d.Seconds())
	weeks := seconds / (7 * 24 * 3600)
	seconds %= 7 * 24 * 3600
	days := seconds / (24 * 3600)
	seconds %= 24 * 3600
	hours := seconds / 3600
	seconds %= 3600
	minutes := seconds / 60
	seconds %= 60

	return fmt.Sprintf("%d weeks, %d days, %d hours, %d minutes, %d seconds",
		weeks, days, hours, minutes, seconds)
}

func formatUptime(uptime time.Duration) string {
	totalMinutes := int(uptime.Minutes())
	days := totalMinutes / (60 * 24)
	hours := (totalMinutes % (60 * 24)) / 60
	minutes := totalMinutes % 60

	if days > 0 {
		return fmt.Sprintf("up %d days, %02d:%02d", days, hours, minutes)
	}
	return fmt.Sprintf("up %02d:%02d", hours, minutes)
}

// readIntFromFile reads a single integer value from a given file path.
// func readIntFromFile(path string) (int64, error) {
// 	data, err := os.ReadFile(path)
// 	if err != nil {
// 		return 0, err
// 	}
// 	return strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
// }

// getCgroupLoad retrieves memory and CPU usage from cgroups v2 and returns a basic load estimate.
func getCgroupLoad() (cpuLoad int64, memoryUsage int64, memoryMax int64, err error) {
	cpuStatPath := "/sys/fs/cgroup/cpu.stat"
	memCurrentPath := "/sys/fs/cgroup/memory.current"
	memMaxPath := "/sys/fs/cgroup/memory.max"

	// Read memory.current
	memoryUsage, err = readIntFromFile(memCurrentPath)
	if err != nil {
		return
	}

	// Read memory.max
	memoryMax, err = readIntFromFile(memMaxPath)
	if err != nil {
		return
	}

	// Read cpu.stat and calculate throttling-based load
	cpuData, err := os.ReadFile(cpuStatPath)
	if err != nil {
		return
	}
	lines := strings.Split(string(cpuData), "\n")
	var throttledUs int64
	for _, line := range lines {
		if strings.HasPrefix(line, "throttled_usec") {
			parts := strings.Fields(line)
			if len(parts) == 2 {
				throttledUs, _ = strconv.ParseInt(parts[1], 10, 64)
			}
			break
		}
	}

	// Synthetic "load": the more throttled time, the higher the load
	cpuLoad = throttledUs / 1_000_000 // convert microseconds to seconds

	return
}

func getCPUUsageMillicores(cpuStatPath string, uptime time.Duration) (float64, error) {
	data, err := os.ReadFile(cpuStatPath)
	if err != nil {
		return 0, err
	}

	lines := strings.Split(string(data), "\n")
	var usageMicros int64
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "usage_usec" {
			usageMicros, err = strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return 0, err
			}
			break
		}
	}

	uptimeSecs := uptime.Seconds()
	if uptimeSecs == 0 {
		return 0, fmt.Errorf("uptime is zero")
	}

	coresUsed := float64(usageMicros) / 1e6 / uptimeSecs // Convert µs to s
	millicores := coresUsed * 1000                       // 1 core = 1000 millicores
	return millicores, nil
}

// func main() {
// 	pretty := flag.Bool("p", false, "Pretty uptime format")
// 	since := flag.Bool("s", false, "Uptime since format")
// 	flag.Parse()

// 	startTime, err := getStartTimeFromProcStat()
// 	if err != nil {
// 		fmt.Printf("Error: %v\n", err)
// 		return
// 	}

// 	if *since {
// 		fmt.Println(startTime.Format("2006-01-02 15:04:05"))
// 	} else if *pretty {
// 		uptime := time.Since(startTime)
// 		fmt.Println(formatPrettyTime(uptime))
// 	} else {
// 		uptime := time.Since(startTime)
// 		now := time.Now()

// 		fmt.Printf("%s %s\n", now.Format("15:04:05"), formatUptime(uptime))

// 		cpuLoad, memoryUsage, memoryMax, err := getCgroupLoad()
// 		if err != nil {
// 			fmt.Printf("Error retrieving cgroup load: %v\n", err)
// 			return
// 		}
// 		fmt.Printf("CPU Load: %d\n", cpuLoad)
// 		fmt.Printf("Memory Usage: %.2f GB\n", float64(memoryUsage)/1e9)
// 		fmt.Printf("Memory Max: %.2f GB\n", float64(memoryMax)/1e9)
// 		fmt.Printf("Memory Usage Percentage: %.2f%%\n", float64(memoryUsage)/float64(memoryMax)*100)

// 		cpuStatPath := "/sys/fs/cgroup/cpu.stat"
// 		cpuUsage, err := getCPUUsageMillicores(cpuStatPath, uptime)
// 		if err != nil {
// 			fmt.Printf("Error retrieving CPU usage: %v\n", err)
// 			return
// 		}
// 		fmt.Printf("CPU Usage: %.2f millicores\n", cpuUsage)
// 	}
// }
