package api

import (
	"github.com/k8shell-io/k8shelld/pkg/api/k8shelldpb"
)

// SystemInfoMetrics represents the system info metrics
type SystemUsage struct {
	Uptime             string  `json:"uptime"`             // time since last start
	CPUUsageMillicores float64 `json:"cpuUsageMillicores"` // CPU usage in millicores
	CPULimitMillicores float64 `json:"cpuLimitMillicores"` // CPU limit in millicores (from kube resource limits)
	MemoryUsageMiB     float64 `json:"memoryUsageMiB"`     // memory usage in MiB
	MemLimitMiB        float64 `json:"memLimitMiB"`        // memory limit in MiB (from kube resource limits)
	CPUAvg1Min         float64 `json:"cpuAvg1min"`         // load average over 1 minute
	CPUAvg5Min         float64 `json:"cpuAvg5min"`         // load average over 5 minutes
	CPUAvg15Min        float64 `json:"cpuAvg15min"`        // load average over 15 minutes
	Users              uint32  `json:"users"`              // number of logged-in users
}

// MountUsage represents the usage statistics for a mounted filesystem
type MountUsage struct {
	MountPoint     string   `json:"mountPoint"`             // path where mounted
	Source         string   `json:"source"`                 // device or source of the mount
	FSType         string   `json:"fsType"`                 // filesystem type (e.g., ext4, tmpfs)
	Options        []string `json:"options"`                // mount options (e.g., ro, rw)
	ReadOnly       bool     `json:"readOnly"`               // is the mount read-only
	IsLikelyTemp   bool     `json:"isLikelyTemp,omitempty"` // overlay/tmpfs/etc.
	TotalBytes     uint64   `json:"totalBytes"`             // total size of the filesystem
	UsedBytes      uint64   `json:"usedBytes"`              // used bytes on the filesystem
	FreeBytes      uint64   `json:"freeBytes"`              // free bytes on the filesystem
	AvailableBytes uint64   `json:"availableBytes"`         // available bytes to non-root users
	TotalInodes    uint64   `json:"totalInodes"`            // total inodes on the filesystem
	FreeInodes     uint64   `json:"freeInodes"`             // free inodes on the filesystem
	DeclaredSize   uint64   `json:"declaredSize"`           // size declared for this mount from the blueprint
}

type DockerUsage struct {
	SocketPath            string `json:"socketPath"`            // path to dockerd socket
	APIVersion            string `json:"apiVersion"`            // dockerd API version
	DockerRootDir         string `json:"dockerRootDir"`         // Docker root directory (DockerRootDir)
	ImagesBytes           uint64 `json:"imagesBytes"`           // total size of all images
	ContainersBytes       uint64 `json:"containersBytes"`       // writable layer only (SizeRw)
	ContainersRootFsBytes uint64 `json:"containersRootFsBytes"` // includes image + writable (SizeRootFs)
	VolumesBytes          uint64 `json:"volumesBytes"`          // total size of all volumes
	BuildCacheBytes       uint64 `json:"buildCacheBytes"`       // total size of build cache
	TotalBytes            uint64 `json:"totalBytes"`            // images + containers(writable) + volumes + build cache
	DeclaredSize          uint64 `json:"declaredSize"`          // size declared for Docker from the blueprint
}

// SystemInfo represents the overall system information
type SystemInfo struct {
	Time   string       `json:"time"`             // current system time
	System *SystemUsage `json:"system"`           // system usage metrics
	Mounts []MountUsage `json:"mounts"`           // list of mount usage statistics
	Docker *DockerUsage `json:"docker,omitempty"` // docker usage statistics
}

// IdentityInfo contains the human-readable JWT identity claims for the workspace user.
type IdentityInfo struct {
	Username     string   `json:"username"`
	Name         string   `json:"name,omitempty"`
	Email        string   `json:"email,omitempty"`
	UID          uint32   `json:"uid"`
	GID          uint32   `json:"gid"`
	Shell        string   `json:"shell,omitempty"`
	Sudo         bool     `json:"sudo"`
	Roles        []string `json:"roles,omitempty"`
	Organization string   `json:"organization,omitempty"`
	Source       string   `json:"source,omitempty"`
	ExpiresAt    string   `json:"expiresAt"`
}

// AppRuntimeStatus represents the lifecycle status reported by AppManager.
type AppRuntimeStatus string

const (
	AppStatusUnknown      AppRuntimeStatus = "-"
	AppStatusNotInstalled AppRuntimeStatus = "NOT-INSTALLED"
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

func SystemInfoToProto(si *SystemInfo) *k8shelldpb.SystemInfoResponse {
	pb := &k8shelldpb.SystemInfoResponse{
		Time: si.Time,
		System: &k8shelldpb.SystemMetrics{
			Uptime:             si.System.Uptime,
			CpuUsageMillicores: si.System.CPUUsageMillicores,
			CpuLimitMillicores: si.System.CPULimitMillicores,
			MemoryUsageMiB:     si.System.MemoryUsageMiB,
			MemLimitMiB:        si.System.MemLimitMiB,
			CpuAvg1Min:         si.System.CPUAvg1Min,
			CpuAvg5Min:         si.System.CPUAvg5Min,
			CpuAvg15Min:        si.System.CPUAvg15Min,
			Users:              si.System.Users,
		},
	}

	for _, mu := range si.Mounts {
		pb.Mounts = append(pb.Mounts, &k8shelldpb.MountUsage{
			MountPoint:     mu.MountPoint,
			Source:         mu.Source,
			FsType:         mu.FSType,
			Options:        mu.Options,
			ReadOnly:       mu.ReadOnly,
			IsLikelyTemp:   mu.IsLikelyTemp,
			TotalBytes:     mu.TotalBytes,
			UsedBytes:      mu.UsedBytes,
			FreeBytes:      mu.FreeBytes,
			AvailableBytes: mu.AvailableBytes,
			TotalInodes:    mu.TotalInodes,
			FreeInodes:     mu.FreeInodes,
			DeclaredSize:   mu.DeclaredSize,
		})
	}

	if si.Docker != nil {
		pb.Docker = &k8shelldpb.DockerUsage{
			SocketPath:            si.Docker.SocketPath,
			ApiVersion:            si.Docker.APIVersion,
			DockerRootDir:         si.Docker.DockerRootDir,
			ImagesBytes:           si.Docker.ImagesBytes,
			ContainersBytes:       si.Docker.ContainersBytes,
			ContainersRootFsBytes: si.Docker.ContainersRootFsBytes,
			VolumesBytes:          si.Docker.VolumesBytes,
			BuildCacheBytes:       si.Docker.BuildCacheBytes,
			TotalBytes:            si.Docker.TotalBytes,
			DeclaredSize:          si.Docker.DeclaredSize,
		}
	}

	return pb
}

func ProtoToSystemInfo(pb *k8shelldpb.SystemInfoResponse) *SystemInfo {
	si := &SystemInfo{
		Time: pb.GetTime(),
		System: &SystemUsage{
			Uptime:             pb.System.GetUptime(),
			CPUUsageMillicores: pb.System.GetCpuUsageMillicores(),
			CPULimitMillicores: pb.System.GetCpuLimitMillicores(),
			MemoryUsageMiB:     pb.System.GetMemoryUsageMiB(),
			MemLimitMiB:        pb.System.GetMemLimitMiB(),
			CPUAvg1Min:         pb.System.GetCpuAvg1Min(),
			CPUAvg5Min:         pb.System.GetCpuAvg5Min(),
			CPUAvg15Min:        pb.System.GetCpuAvg15Min(),
			Users:              pb.System.GetUsers(),
		},
	}

	for _, mu := range pb.Mounts {
		si.Mounts = append(si.Mounts, MountUsage{
			MountPoint:     mu.GetMountPoint(),
			Source:         mu.GetSource(),
			FSType:         mu.GetFsType(),
			Options:        mu.GetOptions(),
			ReadOnly:       mu.GetReadOnly(),
			IsLikelyTemp:   mu.GetIsLikelyTemp(),
			TotalBytes:     mu.GetTotalBytes(),
			UsedBytes:      mu.GetUsedBytes(),
			FreeBytes:      mu.GetFreeBytes(),
			AvailableBytes: mu.GetAvailableBytes(),
			TotalInodes:    mu.GetTotalInodes(),
			FreeInodes:     mu.GetFreeInodes(),
			DeclaredSize:   mu.GetDeclaredSize(),
		})
	}

	if pb.Docker != nil {
		si.Docker = &DockerUsage{
			SocketPath:            pb.Docker.GetSocketPath(),
			APIVersion:            pb.Docker.GetApiVersion(),
			DockerRootDir:         pb.Docker.GetDockerRootDir(),
			ImagesBytes:           pb.Docker.GetImagesBytes(),
			ContainersBytes:       pb.Docker.GetContainersBytes(),
			ContainersRootFsBytes: pb.Docker.GetContainersRootFsBytes(),
			VolumesBytes:          pb.Docker.GetVolumesBytes(),
			BuildCacheBytes:       pb.Docker.GetBuildCacheBytes(),
			TotalBytes:            pb.Docker.GetTotalBytes(),
			DeclaredSize:          pb.Docker.GetDeclaredSize(),
		}
	}

	return si
}
