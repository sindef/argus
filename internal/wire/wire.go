package wire

import (
	"fmt"
	"strings"
	"time"

	"argus/internal/hostd"
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

type TagsRequest struct {
	Tags       string `json:"tags"`
	Folder     string `json:"folder"`
	Attributes string `json:"attributes"`
}

type MoveDiskRequest struct {
	Disk string `json:"disk"`
	Pool string `json:"pool"`
}

type HostSettings struct {
	hostd.Settings
	MigrationBridge string   `json:"migrationBridge"`
	Bridges         []string `json:"bridges"`
	BackupEncrypted bool     `json:"backupEncrypted"`
}

type HostSettingsUpdate struct {
	hostd.SettingsUpdate
	MigrationBridge  *string `json:"migrationBridge,omitempty"`
	BackupPassphrase *string `json:"backupPassphrase,omitempty"`
}

type NetworkRequest struct {
	Name       string `json:"name"`
	VLAN       int    `json:"vlan"`
	Bridge     string `json:"bridge"`
	SpoofCheck bool   `json:"spoofCheck"`
	RateMbps   int    `json:"rateMbps"`
}

func (n NetworkRequest) Kube() kube.Network {
	return kube.Network{Name: n.Name, VLAN: n.VLAN, Bridge: n.Bridge, SpoofCheck: n.SpoofCheck, RateMbps: n.RateMbps}
}

type BackupPolicy struct {
	SnapEvery   int    `json:"snapEvery"`
	SnapKeep    int    `json:"snapKeep"`
	BackupEvery int    `json:"backupEvery"`
	BackupKeep  int    `json:"backupKeep"`
	Target      string `json:"target"`
}

func (p BackupPolicy) String() string {
	return fmt.Sprintf("snap=%d snapkeep=%d backup=%d backupkeep=%d target=%s", p.SnapEvery, p.SnapKeep, p.BackupEvery, p.BackupKeep, p.Target)
}

type VMBackup struct {
	hostd.Backup
	Target string `json:"target"`
}

type VMBackups struct {
	Policy    BackupPolicy    `json:"policy"`
	Enabled   bool            `json:"enabled"`
	Targets   []BackupTarget  `json:"targets"`
	Status    string          `json:"status"`
	Backups   []VMBackup      `json:"backups"`
	Schedules []ScheduledTask `json:"schedules"`
	Error     string          `json:"error"`
}

type BackupTarget struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Endpoint   string `json:"endpoint"`
	Region     string `json:"region,omitempty"`
	Bucket     string `json:"bucket,omitempty"`
	Prefix     string `json:"prefix,omitempty"`
	AccessKey  string `json:"accessKey,omitempty"`
	SecretKey  string `json:"secretKey,omitempty"`
	Passphrase string `json:"passphrase,omitempty"`
	LockDays   int    `json:"lockDays,omitempty"`
}

type ScheduledTask struct {
	Days   []string `json:"days"`
	At     string   `json:"at"`
	Action string   `json:"action"`
}

type StorageInfo struct {
	Pools   []kube.Pool   `json:"pools"`
	Volumes []kube.Volume `json:"volumes"`
}

type ImageImportRequest struct {
	Name     string               `json:"name"`
	Size     string               `json:"size"`
	URL      string               `json:"url"`
	Export   *kube.Export         `json:"export"`
	Registry *kube.RegistrySource `json:"registry"`
}

func (t BackupTarget) Where() string {
	if t.Kind == "s3" {
		return strings.TrimRight(t.Endpoint, "/") + "/" + t.Bucket + "/" + t.Prefix
	}
	return t.Endpoint
}

type VolumeRequest struct {
	Name string `json:"name"`
	Size int    `json:"size"`
	URL  string `json:"url"`
	VM   string `json:"vm"`
	Pool string `json:"pool"`
}

type Task struct {
	ID      string    `json:"id"`
	Kind    string    `json:"kind"`
	VM      string    `json:"vm,omitempty"`
	Object  string    `json:"object,omitempty"`
	Host    string    `json:"host,omitempty"`
	Node    string    `json:"node,omitempty"`
	User    string    `json:"user,omitempty"`
	Status  string    `json:"status"`
	Started time.Time `json:"started"`
	Ended   time.Time `json:"ended,omitzero"`
	Done    bool      `json:"done"`
	Failed  bool      `json:"failed"`
	Hold    bool      `json:"hold,omitempty"`
}
