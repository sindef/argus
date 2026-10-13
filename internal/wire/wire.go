package wire

import (
	"strings"

	"argus/internal/kube"
)

type ExportRequest struct {
	BaseURL string `json:"baseURL"`
}

type ImportRequest struct {
	Name      string      `json:"name"`
	Operation string      `json:"operation"`
	Export    kube.Export `json:"export"`
	Pool      string      `json:"pool,omitempty"`
	Template  bool        `json:"template,omitempty"`
}

type OperationRequest struct {
	Operation string `json:"operation"`
}

type HARequest struct {
	Enabled bool `json:"enabled"`
}

type Heartbeat struct {
	Owned    []string          `json:"owned"`
	Replicas map[string]string `json:"replicas"`
}

type ReplicaRequest struct {
	Export  kube.Export       `json:"export"`
	Mapping map[string]string `json:"mapping"`
}

type ZFSSnapshotRequest struct {
	Keep []string `json:"keep"`
}

type ZFSSnapshot struct {
	Snap string         `json:"snap"`
	Spec map[string]any `json:"spec"`
}

type ReceiveRequest struct {
	Name        string         `json:"name"`
	MigrationID string         `json:"migrationID"`
	Spec        map[string]any `json:"spec"`
}

type ReceiveResponse struct {
	SyncAddress string `json:"syncAddress"`
}

type SendRequest struct {
	MigrationID string `json:"migrationID"`
	ConnectURL  string `json:"connectURL"`
}

type MetaRequest struct {
	Value    string `json:"value"`
	Template bool   `json:"template,omitempty"`
}

type ExternalCARequest struct {
	Bundle string `json:"bundle"`
}

type RetireTokenRequest struct {
	Name string `json:"name"`
}

type CPUBaselineRequest struct {
	Model string `json:"model"`
}

type HostState string

const (
	HostIdle      HostState = "idle"
	HostBusy      HostState = "busy"
	HostFailed    HostState = "failed"
	HostCancelled HostState = "cancelled"
)

type HostStatus struct {
	Status  string    `json:"status"`
	State   HostState `json:"state"`
	Version string    `json:"version"`
	Running int       `json:"running"`
	Ready   bool      `json:"ready"`
	Reason  string    `json:"reason"`
}

func StateOf(status string) HostState {
	switch {
	case status == "":
		return HostIdle
	case strings.HasPrefix(status, "Update failed"), strings.HasPrefix(status, "Unknown request"):
		return HostFailed
	case strings.HasPrefix(status, "Cancelled"):
		return HostCancelled
	}
	return HostBusy
}
