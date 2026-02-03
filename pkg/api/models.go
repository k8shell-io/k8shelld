package api

import (
	"github.com/k8shell-io/k8shelld/pkg/api/k8shelldpb"
)

// SystemInfoMetrics represents the system info metrics
type SystemUsage struct {
	Uptime             string  `json:"uptime"`
	CPUUsageMillicores float64 `json:"cpuUsageMillicores"`
	CPULimitMillicores float64 `json:"cpuLimitMillicores"`
	MemoryUsageMiB     float64 `json:"memoryUsageMiB"`
	MemLimitMiB        float64 `json:"memLimitMiB"`
	CPUAvg1Min         float64 `json:"cpuAvg1min"`
	CPUAvg5Min         float64 `json:"cpuAvg5min"`
	CPUAvg15Min        float64 `json:"cpuAvg15min"`
	Users              int     `json:"users"`
}

type MountUsage struct {
	MountPoint     string   `json:"mountPoint"`
	Source         string   `json:"source"`
	FSType         string   `json:"fsType"`
	Options        []string `json:"options"`
	ReadOnly       bool     `json:"readOnly"`
	IsLikelyTemp   bool     `json:"isLikelyTemp,omitempty"` // overlay/tmpfs/etc.
	TotalBytes     uint64   `json:"totalBytes"`
	UsedBytes      uint64   `json:"usedBytes"`
	FreeBytes      uint64   `json:"freeBytes"`
	AvailableBytes uint64   `json:"availableBytes"`
	TotalInodes    uint64   `json:"totalInodes"`
	FreeInodes     uint64   `json:"freeInodes"`
	DeclaredSize   uint64   `json:"declaredSize"`
}

type DockerUsage struct {
	SocketPath            string `json:"socketPath"`
	APIVersion            string `json:"apiVersion"`
	DockerRootDir         string `json:"dockerRootDir"`
	ImagesBytes           uint64 `json:"imagesBytes"`
	ContainersBytes       uint64 `json:"containersBytes"`       // writable layer only (SizeRw)
	ContainersRootFsBytes uint64 `json:"containersRootFsBytes"` // includes image + writable (SizeRootFs)
	VolumesBytes          uint64 `json:"volumesBytes"`
	BuildCacheBytes       uint64 `json:"buildCacheBytes"`
	TotalBytes            uint64 `json:"totalBytes"` // images + containers(writable) + volumes + build cache
	DeclaredSize          uint64 `json:"declaredSize"`
}

type SystemInfo struct {
	Time   string       `json:"time"`
	System SystemUsage  `json:"system"`
	Mounts []MountUsage `json:"mounts"`
	Docker *DockerUsage `json:"docker,omitempty"`
}

// AppRuntimeStatus represents the lifecycle status reported by AppManager.
type AppRuntimeStatus string

const (
	AppStatusUnknown      AppRuntimeStatus = "-"
	AppStatusNotInstalled AppRuntimeStatus = "UNINSTALLED"
	AppStatusInstalling   AppRuntimeStatus = "INSTALLING"
	AppStatusNotStarted   AppRuntimeStatus = "STOPPED"
	AppStatusPending      AppRuntimeStatus = "PENDING"
	AppStatusRunning      AppRuntimeStatus = "RUNNING"
	AppStatusInvalid      AppRuntimeStatus = "INVALID"
)

func (s AppRuntimeStatus) String() string { return string(s) }

func (s AppRuntimeStatus) IsValid() bool {
	switch s {
	case AppStatusUnknown,
		AppStatusInstalling,
		AppStatusNotStarted,
		AppStatusNotInstalled,
		AppStatusInvalid,
		AppStatusPending,
		AppStatusRunning:
		return true
	default:
		return false
	}
}

// AppStatus represents the current status of an application
type AppStatus struct {
	Name     string           `json:"name"`
	Status   AppRuntimeStatus `json:"status"`
	Version  string           `json:"version"`
	Port     int              `json:"port"`
	PID      int              `json:"pid"`
	Age      string           `json:"age"`
	Restarts int              `json:"restarts"`
	Protocol string           `json:"protocol"`
}

func AppStatusToProto(u *AppStatus) *k8shelldpb.AppStatus {
	return &k8shelldpb.AppStatus{
		Name:     u.Name,
		Status:   u.Status.String(),
		Version:  u.Version,
		Port:     safeIntToInt32(u.Port),
		Pid:      safeIntToInt32(u.PID),
		Age:      u.Age,
		Restarts: safeIntToInt32(u.Restarts),
		Protocol: u.Protocol,
	}
}

func AppStatusFromProto(u *k8shelldpb.AppStatus) *AppStatus {
	status := AppRuntimeStatus(u.GetStatus())
	if !status.IsValid() {
		status = AppStatusUnknown
	}
	return &AppStatus{
		Name:     u.GetName(),
		Status:   status,
		Version:  u.GetVersion(),
		Port:     int(u.GetPort()),
		PID:      int(u.GetPid()),
		Age:      u.GetAge(),
		Restarts: int(u.GetRestarts()),
		Protocol: u.GetProtocol(),
	}
}

func LogTypeToProto(logType string) k8shelldpb.LogType {
	switch logType {
	case "install":
		return k8shelldpb.LogType_LOG_TYPE_INSTALL
	case "app":
		return k8shelldpb.LogType_LOG_TYPE_APP
	default:
		return k8shelldpb.LogType_LOG_TYPE_INSTALL
	}
}

func LogTypeFromProto(logType k8shelldpb.LogType) string {
	switch logType {
	case k8shelldpb.LogType_LOG_TYPE_INSTALL:
		return "install"
	case k8shelldpb.LogType_LOG_TYPE_APP:
		return "app"
	default:
		return "install"
	}
}

// safeIntToInt32 converts int to int32, clamping to int32 max/min.
func safeIntToInt32(v int) int32 {
	if v > 2147483647 {
		return 2147483647
	}
	if v < -2147483648 {
		return -2147483648
	}
	return int32(v)
}
