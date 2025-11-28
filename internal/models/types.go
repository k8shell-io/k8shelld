package models

var RESTAPIUnixSocket = "/var/run/k8shelld.sock"

// SystemInfoResponse represents the response for system info
type SystemInfoResponse struct {
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

// K8shellFileValidationResponse represents the response for k8shell file validation
type K8shellFileValidationResponse struct {
	Status   string   `json:"status"`
	Filename string   `json:"filename"`
	Errors   []string `json:"errors,omitempty"`
}

// AppStatus represents the current status of an application
type AppStatus struct {
	Name        string `json:"name"`
	Status      string `json:"status"`
	Version     string `json:"version"`
	ListenPort  int    `json:"listen_port"`
	PID         int    `json:"pid"`
	RunningTime string `json:"running_time"`
}
