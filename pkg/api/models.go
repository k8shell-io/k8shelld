package api

import (
	"github.com/k8shell-io/k8shelld/pkg/api/k8shelldpb"
)

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

// AppStatus represents the current status of an application
type AppStatus struct {
	Name     string `json:"name"`
	Status   string `json:"status"`
	Version  string `json:"version"`
	Port     int    `json:"port"`
	PID      int    `json:"pid"`
	Age      string `json:"age"`
	Restarts int    `json:"restarts"`
	Protocol string `json:"protocol"`
}

func AppStatusToProto(u *AppStatus) *k8shelldpb.AppStatus {
	return &k8shelldpb.AppStatus{
		Name:     u.Name,
		Status:   u.Status,
		Version:  u.Version,
		Port:     safeIntToInt32(u.Port),
		Pid:      safeIntToInt32(u.PID),
		Age:      u.Age,
		Restarts: safeIntToInt32(u.Restarts),
		Protocol: u.Protocol,
	}
}

func AppStatusFromProto(u *k8shelldpb.AppStatus) *AppStatus {
	return &AppStatus{
		Name:     u.GetName(),
		Status:   u.GetStatus(),
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
