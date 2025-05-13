package common

var DefaultRESTAPIUnixSocket = "/var/run/k8shelld.sock"

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
}
