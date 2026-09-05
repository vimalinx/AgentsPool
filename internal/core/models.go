package core

import "time"

const StateSchemaVersion = 5

const (
	NodeOnline   = "online"
	NodeOffline  = "offline"
	NodeDraining = "draining"
	NodeDisabled = "disabled"
)

const (
	DeviceDescriptionSourceHuman = "human"
	DeviceDescriptionSourceAgent = "agent"
)

const (
	DeviceAvailabilityUnknown   = "unknown"
	DeviceAvailabilityAlwaysOn  = "always_on"
	DeviceAvailabilityScheduled = "scheduled"
	DeviceAvailabilityOnDemand  = "on_demand"
)

const (
	DeviceServicePolicyUnknown   = "unknown"
	DeviceServicePolicyPreferred = "preferred"
	DeviceServicePolicyAllowed   = "allowed"
	DeviceServicePolicyAvoid     = "avoid"
	DeviceServicePolicyForbidden = "forbidden"
)

const (
	ResourceLeaseActive   = "active"
	ResourceLeaseReleased = "released"
	ResourceLeaseExpired  = "expired"
)

const (
	SandboxPending   = "pending"
	SandboxReady     = "ready"
	SandboxStopped   = "stopped"
	SandboxError     = "error"
	SandboxDestroyed = "destroyed"
)

const (
	SandboxOperationQueued    = "queued"
	SandboxOperationClaimed   = "claimed"
	SandboxOperationRunning   = "running"
	SandboxOperationOrphaned  = "orphaned"
	SandboxOperationSucceeded = "succeeded"
	SandboxOperationFailed    = "failed"
	SandboxOperationCancelled = "cancelled"
)

const (
	TaskQueued      = "queued"
	TaskLeased      = "leased"
	TaskRunning     = "running"
	TaskRetryWait   = "retry_wait"
	TaskSucceeded   = "succeeded"
	TaskFailed      = "failed"
	TaskCancelled   = "cancelled"
	TaskDeadLetter  = "dead_letter"
	TaskNeedsReview = "needs_review"
)

const (
	LeaseActive    = "active"
	LeaseCompleted = "completed"
	LeaseFailed    = "failed"
	LeaseExpired   = "expired"
	LeaseCancelled = "cancelled"
)

const (
	ExecutionLeased    = "leased"
	ExecutionRunning   = "running"
	ExecutionSucceeded = "succeeded"
	ExecutionFailed    = "failed"
	ExecutionCancelled = "cancelled"
	ExecutionOrphaned  = "orphaned"
)

const (
	SideEffectNone          = "none"
	SideEffectIdempotent    = "idempotent"
	SideEffectNonIdempotent = "non_idempotent"
)

const (
	ExecutorShell   = "shell"
	ExecutorProcess = "process"
	ExecutorDocker  = "docker"
)

type GPUInfo struct {
	Index          int    `json:"index"`
	Name           string `json:"name"`
	UUID           string `json:"uuid,omitempty"`
	MemoryTotalMB  int64  `json:"memory_total_mb"`
	MemoryFreeMB   int64  `json:"memory_free_mb"`
	DriverVersion  string `json:"driver_version,omitempty"`
	ComputeRuntime string `json:"compute_runtime,omitempty"`
}

type NodeCapabilities struct {
	Hostname      string            `json:"hostname"`
	OS            string            `json:"os"`
	Arch          string            `json:"arch"`
	CPUThreads    int               `json:"cpu_threads"`
	MemoryTotalMB int64             `json:"memory_total_mb"`
	DiskTotalMB   int64             `json:"disk_total_mb"`
	GPUs          []GPUInfo         `json:"gpus,omitempty"`
	Capabilities  []string          `json:"capabilities"`
	Labels        map[string]string `json:"labels,omitempty"`
	Queues        []string          `json:"queues,omitempty"`
	AgentVersion  string            `json:"agent_version"`
}

type NodeResources struct {
	CPUPercent        float64   `json:"cpu_percent"`
	Load1             float64   `json:"load_1"`
	MemoryAvailableMB int64     `json:"memory_available_mb"`
	DiskAvailableMB   int64     `json:"disk_available_mb"`
	GPUs              []GPUInfo `json:"gpus,omitempty"`
	ActiveJobs        int       `json:"active_jobs"`
	MaxConcurrency    int       `json:"max_concurrency"`
	ObservedAt        time.Time `json:"observed_at"`
}

// DeviceDescription keeps human intent separate from Agent recommendations.
// It is control-plane state, so Worker heartbeats cannot overwrite it. Human
// entries are authoritative; Agent entries are advisory and record the device
// registry revision on which the recommendation was based.
type DeviceDescription struct {
	Source             string    `json:"source"`
	Author             string    `json:"author"`
	Description        string    `json:"description,omitempty"`
	Availability       string    `json:"availability,omitempty"`
	AvailabilityNotes  string    `json:"availability_notes,omitempty"`
	ServicePolicy      string    `json:"service_policy,omitempty"`
	PreferredWorkloads []string  `json:"preferred_workloads,omitempty"`
	AvoidWorkloads     []string  `json:"avoid_workloads,omitempty"`
	Constraints        []string  `json:"constraints,omitempty"`
	BasisRevision      int64     `json:"basis_revision"`
	UpdatedAt          time.Time `json:"updated_at"`
}

type Node struct {
	ID           string              `json:"id"`
	MachineID    string              `json:"machine_id"`
	Name         string              `json:"name"`
	TokenHash    string              `json:"token_hash"`
	Status       string              `json:"status"`
	Capabilities NodeCapabilities    `json:"capabilities"`
	Resources    NodeResources       `json:"resources"`
	Descriptions []DeviceDescription `json:"descriptions,omitempty"`
	RegisteredAt time.Time           `json:"registered_at"`
	LastSeenAt   time.Time           `json:"last_seen_at"`
	UpdatedAt    time.Time           `json:"updated_at"`
	DisabledAt   *time.Time          `json:"disabled_at,omitempty"`
}

type Requirements struct {
	Capabilities     []string          `json:"capabilities,omitempty"`
	OS               string            `json:"os,omitempty"`
	Arch             string            `json:"arch,omitempty"`
	MinCPUThreads    int               `json:"min_cpu_threads,omitempty"`
	MinMemoryMB      int64             `json:"min_memory_mb,omitempty"`
	MinDiskMB        int64             `json:"min_disk_mb,omitempty"`
	Labels           map[string]string `json:"labels,omitempty"`
	AllowedNodeIDs   []string          `json:"allowed_node_ids,omitempty"`
	ExcludedNodeIDs  []string          `json:"excluded_node_ids,omitempty"`
	PreferredNodeIDs []string          `json:"preferred_node_ids,omitempty"`
}

type ExecutorSpec struct {
	Kind         string            `json:"kind"`
	Script       string            `json:"script,omitempty"`
	Command      []string          `json:"command,omitempty"`
	Image        string            `json:"image,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
	WorkDir      string            `json:"work_dir,omitempty"`
	Network      string            `json:"network,omitempty"`
	CPULimit     float64           `json:"cpu_limit,omitempty"`
	MemoryMB     int64             `json:"memory_mb,omitempty"`
	User         string            `json:"user,omitempty"`
	ReadOnlyRoot bool              `json:"read_only_root,omitempty"`
}

type ArtifactInput struct {
	ArtifactID string `json:"artifact_id"`
	Path       string `json:"path"`
	Executable bool   `json:"executable,omitempty"`
}

type OutputRule struct {
	Pattern   string `json:"pattern"`
	Required  bool   `json:"required,omitempty"`
	MediaType string `json:"media_type,omitempty"`
}

type TaskSpec struct {
	Name                string            `json:"name"`
	Queue               string            `json:"queue,omitempty"`
	Priority            int               `json:"priority,omitempty"`
	MaxAttempts         int               `json:"max_attempts,omitempty"`
	TimeoutSeconds      int               `json:"timeout_seconds,omitempty"`
	RetryBackoffSeconds int               `json:"retry_backoff_seconds,omitempty"`
	RetryOnFailure      bool              `json:"retry_on_failure,omitempty"`
	SideEffectMode      string            `json:"side_effect_mode,omitempty"`
	IdempotencyKey      string            `json:"idempotency_key,omitempty"`
	Requirements        Requirements      `json:"requirements,omitempty"`
	Executor            ExecutorSpec      `json:"executor"`
	Inputs              []ArtifactInput   `json:"inputs,omitempty"`
	Outputs             []OutputRule      `json:"outputs,omitempty"`
	Labels              map[string]string `json:"labels,omitempty"`
	Metadata            map[string]string `json:"metadata,omitempty"`
}

type TaskResult struct {
	ExitCode          int               `json:"exit_code"`
	Summary           string            `json:"summary,omitempty"`
	OutputArtifactIDs []string          `json:"output_artifact_ids,omitempty"`
	Metadata          map[string]string `json:"metadata,omitempty"`
}

type Task struct {
	ID              string      `json:"id"`
	Spec            TaskSpec    `json:"spec"`
	Status          string      `json:"status"`
	Attempts        int         `json:"attempts"`
	AssignedNodeID  string      `json:"assigned_node_id,omitempty"`
	ActiveLeaseID   string      `json:"active_lease_id,omitempty"`
	CancelRequested bool        `json:"cancel_requested,omitempty"`
	CreatedAt       time.Time   `json:"created_at"`
	UpdatedAt       time.Time   `json:"updated_at"`
	StartedAt       *time.Time  `json:"started_at,omitempty"`
	FinishedAt      *time.Time  `json:"finished_at,omitempty"`
	NextEligibleAt  *time.Time  `json:"next_eligible_at,omitempty"`
	LastError       string      `json:"last_error,omitempty"`
	Result          *TaskResult `json:"result,omitempty"`
}

type Lease struct {
	ID            string     `json:"id"`
	TaskID        string     `json:"task_id"`
	NodeID        string     `json:"node_id"`
	ExecutionID   string     `json:"execution_id"`
	TokenHash     string     `json:"token_hash"`
	Status        string     `json:"status"`
	TTLSeconds    int        `json:"ttl_seconds"`
	CreatedAt     time.Time  `json:"created_at"`
	LastRenewedAt time.Time  `json:"last_renewed_at"`
	ExpiresAt     time.Time  `json:"expires_at"`
	FinishedAt    *time.Time `json:"finished_at,omitempty"`
}

type Execution struct {
	ID                string     `json:"id"`
	TaskID            string     `json:"task_id"`
	NodeID            string     `json:"node_id"`
	LeaseID           string     `json:"lease_id"`
	Attempt           int        `json:"attempt"`
	Status            string     `json:"status"`
	CreatedAt         time.Time  `json:"created_at"`
	StartedAt         *time.Time `json:"started_at,omitempty"`
	FinishedAt        *time.Time `json:"finished_at,omitempty"`
	ExitCode          *int       `json:"exit_code,omitempty"`
	Error             string     `json:"error,omitempty"`
	OutputArtifactIDs []string   `json:"output_artifact_ids,omitempty"`
	LogSequence       int64      `json:"log_sequence"`
}

type Artifact struct {
	ID                  string            `json:"id"`
	SHA256              string            `json:"sha256"`
	Name                string            `json:"name"`
	MediaType           string            `json:"media_type"`
	Size                int64             `json:"size"`
	StoragePath         string            `json:"storage_path"`
	CreatedAt           time.Time         `json:"created_at"`
	ProducerTaskID      string            `json:"producer_task_id,omitempty"`
	ProducerExecutionID string            `json:"producer_execution_id,omitempty"`
	Metadata            map[string]string `json:"metadata,omitempty"`
}

type Event struct {
	ID           string            `json:"id"`
	Timestamp    time.Time         `json:"timestamp"`
	Type         string            `json:"type"`
	Actor        string            `json:"actor"`
	ResourceType string            `json:"resource_type,omitempty"`
	ResourceID   string            `json:"resource_id,omitempty"`
	Message      string            `json:"message"`
	Data         map[string]string `json:"data,omitempty"`
}

// ResourceRequest is deliberately GPU-free. GPU inventory is telemetry only
// and cannot be reserved or exposed through the sandbox API.
type ResourceRequest struct {
	CPUMillis int64 `json:"cpu_millis"`
	MemoryMB  int64 `json:"memory_mb"`
	DiskMB    int64 `json:"disk_mb"`
}

type ResourceLease struct {
	ID               string          `json:"id"`
	NodeID           string          `json:"node_id"`
	SandboxID        string          `json:"sandbox_id,omitempty"`
	Owner            string          `json:"owner,omitempty"`
	TokenHash        string          `json:"token_hash"`
	Status           string          `json:"status"`
	Resources        ResourceRequest `json:"resources"`
	ComputeReleased  bool            `json:"compute_released,omitempty"`
	TTLSeconds       int             `json:"ttl_seconds,omitempty"`
	CreatedAt        time.Time       `json:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
	LastRenewedAt    time.Time       `json:"last_renewed_at"`
	ExpiresAt        *time.Time      `json:"expires_at,omitempty"`
	ReleasedAt       *time.Time      `json:"released_at,omitempty"`
	RegistryRevision int64           `json:"registry_revision"`
}

// ActiveAt reports whether a resource lease is usable at the supplied time.
// A nil expiry is intentional: ResourceLease lifetime is Agent-controlled, and
// omitted/zero ttl_seconds means that no time deadline was requested.
func (lease *ResourceLease) ActiveAt(now time.Time) bool {
	return lease != nil && lease.Status == ResourceLeaseActive && (lease.ExpiresAt == nil || lease.ExpiresAt.After(now))
}

type SandboxPermissions struct {
	Exec      bool `json:"exec"`
	DataRead  bool `json:"data_read"`
	DataWrite bool `json:"data_write"`
	Network   bool `json:"network"`
}

type SandboxEndpoint struct {
	Name      string `json:"name"`
	Protocol  string `json:"protocol"`
	Port      int    `json:"port"`
	URL       string `json:"url"`
	HealthURL string `json:"health_url,omitempty"`
}

type Sandbox struct {
	ID                 string             `json:"id"`
	Name               string             `json:"name"`
	NodeID             string             `json:"node_id"`
	ResourceLeaseID    string             `json:"resource_lease_id"`
	TokenHash          string             `json:"token_hash"`
	Status             string             `json:"status"`
	Permissions        SandboxPermissions `json:"permissions"`
	WorkspaceVolumeID  string             `json:"workspace_volume_id"`
	HomeVolumeID       string             `json:"home_volume_id"`
	CreatedAt          time.Time          `json:"created_at"`
	UpdatedAt          time.Time          `json:"updated_at"`
	LastUsedAt         *time.Time         `json:"last_used_at,omitempty"`
	ServiceOperationID string             `json:"service_operation_id,omitempty"`
	PublishedEndpoints []SandboxEndpoint  `json:"published_endpoints,omitempty"`
	Error              string             `json:"error,omitempty"`
}

type SandboxVolume struct {
	ID        string    `json:"id"`
	SandboxID string    `json:"sandbox_id"`
	NodeID    string    `json:"node_id"`
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`
	SizeMB    int64     `json:"size_mb"`
	MountPath string    `json:"mount_path"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type SandboxExecSpec struct {
	Command        []string          `json:"command"`
	Env            map[string]string `json:"env,omitempty"`
	WorkingDir     string            `json:"working_dir,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty"`
	Endpoints      []SandboxEndpoint `json:"endpoints,omitempty"`
}

type SandboxLeasePolicy struct {
	Active    bool       `json:"active"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

type SandboxFileSpec struct {
	Path       string `json:"path"`
	ArtifactID string `json:"artifact_id,omitempty"`
	Executable bool   `json:"executable,omitempty"`
}

type SandboxOperation struct {
	ID                string           `json:"id"`
	SandboxID         string           `json:"sandbox_id"`
	NodeID            string           `json:"node_id"`
	Kind              string           `json:"kind"`
	Status            string           `json:"status"`
	Exec              *SandboxExecSpec `json:"exec,omitempty"`
	File              *SandboxFileSpec `json:"file,omitempty"`
	Resources         *ResourceRequest `json:"resources,omitempty"`
	ClaimTokenHash    string           `json:"claim_token_hash,omitempty"`
	CreatedAt         time.Time        `json:"created_at"`
	UpdatedAt         time.Time        `json:"updated_at"`
	ClaimedAt         *time.Time       `json:"claimed_at,omitempty"`
	StartedAt         *time.Time       `json:"started_at,omitempty"`
	FinishedAt        *time.Time       `json:"finished_at,omitempty"`
	CancelRequested   bool             `json:"cancel_requested,omitempty"`
	CancelRequestedAt *time.Time       `json:"cancel_requested_at,omitempty"`
	CancelledAt       *time.Time       `json:"cancelled_at,omitempty"`
	ExitCode          *int             `json:"exit_code,omitempty"`
	Stdout            string           `json:"stdout,omitempty"`
	Stderr            string           `json:"stderr,omitempty"`
	OutputTruncated   bool             `json:"output_truncated,omitempty"`
	Error             string           `json:"error,omitempty"`
	ResultArtifactID  string           `json:"result_artifact_id,omitempty"`
}

func (operation *SandboxOperation) Terminal() bool {
	if operation == nil {
		return true
	}
	switch operation.Status {
	case SandboxOperationSucceeded, SandboxOperationFailed, SandboxOperationCancelled:
		return true
	default:
		return false
	}
}

type RegistryEvent struct {
	Revision     int64             `json:"revision"`
	Timestamp    time.Time         `json:"timestamp"`
	Type         string            `json:"type"`
	ResourceType string            `json:"resource_type"`
	ResourceID   string            `json:"resource_id"`
	Data         map[string]string `json:"data,omitempty"`
}

type State struct {
	SchemaVersion     int                          `json:"schema_version"`
	RegistryRevision  int64                        `json:"registry_revision"`
	RegistryEvents    []RegistryEvent              `json:"registry_events"`
	Nodes             map[string]*Node             `json:"nodes"`
	Tasks             map[string]*Task             `json:"tasks"`
	Leases            map[string]*Lease            `json:"leases"`
	ResourceLeases    map[string]*ResourceLease    `json:"resource_leases"`
	Sandboxes         map[string]*Sandbox          `json:"sandboxes"`
	SandboxVolumes    map[string]*SandboxVolume    `json:"sandbox_volumes"`
	SandboxOperations map[string]*SandboxOperation `json:"sandbox_operations"`
	Executions        map[string]*Execution        `json:"executions"`
	Artifacts         map[string]*Artifact         `json:"artifacts"`
	Idempotency       map[string]string            `json:"idempotency"`
	Events            []Event                      `json:"events"`
}

func NewState() State {
	return State{
		SchemaVersion:     StateSchemaVersion,
		RegistryEvents:    []RegistryEvent{},
		Nodes:             map[string]*Node{},
		Tasks:             map[string]*Task{},
		Leases:            map[string]*Lease{},
		ResourceLeases:    map[string]*ResourceLease{},
		Sandboxes:         map[string]*Sandbox{},
		SandboxVolumes:    map[string]*SandboxVolume{},
		SandboxOperations: map[string]*SandboxOperation{},
		Executions:        map[string]*Execution{},
		Artifacts:         map[string]*Artifact{},
		Idempotency:       map[string]string{},
		Events:            []Event{},
	}
}

func (s *State) EnsureMaps() {
	if s.RegistryEvents == nil {
		s.RegistryEvents = []RegistryEvent{}
	}
	if s.Nodes == nil {
		s.Nodes = map[string]*Node{}
	}
	if s.Tasks == nil {
		s.Tasks = map[string]*Task{}
	}
	if s.Leases == nil {
		s.Leases = map[string]*Lease{}
	}
	if s.ResourceLeases == nil {
		s.ResourceLeases = map[string]*ResourceLease{}
	}
	if s.Sandboxes == nil {
		s.Sandboxes = map[string]*Sandbox{}
	}
	if s.SandboxVolumes == nil {
		s.SandboxVolumes = map[string]*SandboxVolume{}
	}
	if s.SandboxOperations == nil {
		s.SandboxOperations = map[string]*SandboxOperation{}
	}
	if s.Executions == nil {
		s.Executions = map[string]*Execution{}
	}
	if s.Artifacts == nil {
		s.Artifacts = map[string]*Artifact{}
	}
	if s.Idempotency == nil {
		s.Idempotency = map[string]string{}
	}
	if s.Events == nil {
		s.Events = []Event{}
	}
	if s.SchemaVersion == 0 {
		s.SchemaVersion = StateSchemaVersion
	}
}

func IsTerminalTaskStatus(status string) bool {
	switch status {
	case TaskSucceeded, TaskFailed, TaskCancelled, TaskDeadLetter, TaskNeedsReview:
		return true
	default:
		return false
	}
}
