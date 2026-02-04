package models

var RESTAPIUnixSocket = "/var/run/k8shelld.sock"

// K8shellFileValidationResponse represents the response for k8shell file validation
type K8shellFileValidationResponse struct {
	Status   string   `json:"status"`
	Filename string   `json:"filename"`
	Errors   []string `json:"errors,omitempty"`
}
