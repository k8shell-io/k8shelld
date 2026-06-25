// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package models

import "time"

var RESTAPIUnixSocket = "/var/run/k8shelld.sock"

// InitScriptStatus represents the lifecycle state of an init script.
type InitScriptStatus string

const (
	InitScriptPending   InitScriptStatus = "pending"
	InitScriptRunning   InitScriptStatus = "running"
	InitScriptCompleted InitScriptStatus = "completed"
)

// InitScriptState holds the observable state of one init script.
type InitScriptState struct {
	Name       string           `json:"name"`
	Status     InitScriptStatus `json:"status"`
	StartedAt  *time.Time       `json:"started_at,omitempty"`
	FinishedAt *time.Time       `json:"finished_at,omitempty"`
	HasError   bool             `json:"has_error"`
}

// K8shellFileValidationResponse represents the response for k8shell file validation
type K8shellFileValidationResponse struct {
	Status   string   `json:"status"`
	Filename string   `json:"filename"`
	Errors   []string `json:"errors,omitempty"`
}
