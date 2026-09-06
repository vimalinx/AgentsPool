package server

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/vimalinx/AgentsPool/internal/core"
)

type sandboxCreateRequest struct {
	Name            string                  `json:"name"`
	ResourceLeaseID string                  `json:"resource_lease_id"`
	Permissions     core.SandboxPermissions `json:"permissions"`
}

type sandboxResizeRequest struct {
	ExpectedRevision int64                `json:"expected_revision"`
	Resources        core.ResourceRequest `json:"resources"`
}

type sandboxRebindRequest struct {
	ResourceLeaseID string `json:"resource_lease_id"`
}

type sandboxOperationClaimResponse struct {
	Claimed              bool                   `json:"claimed"`
	Operation            *core.SandboxOperation `json:"operation,omitempty"`
	OperationToken       string                 `json:"operation_token,omitempty"`
	Sandbox              *core.Sandbox          `json:"sandbox,omitempty"`
	Resources            *core.ResourceRequest  `json:"resources,omitempty"`
	ResourceLeaseExpires *time.Time             `json:"resource_lease_expires_at,omitempty"`
}

type sandboxOperationCompleteRequest struct {
	Success         bool   `json:"success"`
	Cancelled       bool   `json:"cancelled,omitempty"`
	ExitCode        *int   `json:"exit_code,omitempty"`
	Stdout          string `json:"stdout,omitempty"`
	Stderr          string `json:"stderr,omitempty"`
	Error           string `json:"error,omitempty"`
	OutputTruncated bool   `json:"output_truncated,omitempty"`
}

func publicSandbox(sandbox *core.Sandbox) core.Sandbox {
	copy := cloneViaJSON(*sandbox)
	copy.TokenHash = ""
	return copy
}

func publicSandboxOperation(operation *core.SandboxOperation) core.SandboxOperation {
	copy := cloneViaJSON(*operation)
	copy.ClaimTokenHash = ""
	return copy
}

func queueSandboxOperation(state *core.State, sandbox *core.Sandbox, kind string, exec *core.SandboxExecSpec, now time.Time) (*core.SandboxOperation, error) {
	for _, current := range state.SandboxOperations {
		if current.SandboxID != sandbox.ID || current.Terminal() {
			continue
		}
		if (kind == "stop" || kind == "destroy") && current.Kind == "exec" && current.CancelRequested {
			continue
		}
		return nil, fmt.Errorf("sandbox operation %s is still %s; wait or stop it first", current.ID, current.Status)
	}
	operationID, err := core.NewID("sbxop")
	if err != nil {
		return nil, err
	}
	operation := &core.SandboxOperation{
		ID: operationID, SandboxID: sandbox.ID, NodeID: sandbox.NodeID, Kind: kind,
		Status: core.SandboxOperationQueued, Exec: exec, CreatedAt: now, UpdatedAt: now,
	}
	state.SandboxOperations[operationID] = operation
	return operation, nil
}

func queueSandboxFileOperation(state *core.State, sandbox *core.Sandbox, kind string, file core.SandboxFileSpec, now time.Time) (*core.SandboxOperation, error) {
	operation, err := queueSandboxOperation(state, sandbox, kind, nil, now)
	if err != nil {
		return nil, err
	}
	operation.File = &file
	return operation, nil
}

func (s *Server) handleSandboxCreate(w http.ResponseWriter, r *http.Request) {
	var request sandboxCreateRequest
	if !decodeJSON(w, r, s.cfg.MaxRequestBytes, &request) {
		return
	}
	request.Name = strings.TrimSpace(request.Name)
	request.ResourceLeaseID = strings.TrimSpace(request.ResourceLeaseID)
	if request.Name == "" || len(request.Name) > 100 {
		writeAPIError(w, http.StatusBadRequest, "invalid_name", "name is required and must not exceed 100 characters")
		return
	}
	if request.ResourceLeaseID == "" {
		writeAPIError(w, http.StatusBadRequest, "invalid_resource_lease_id", "resource_lease_id is required")
		return
	}
	if _, ok := s.authenticateResourceLease(w, r, request.ResourceLeaseID); !ok {
		return
	}
	if !request.Permissions.Exec && !request.Permissions.DataRead && !request.Permissions.DataWrite {
		writeAPIError(w, http.StatusBadRequest, "invalid_permissions", "at least one sandbox permission is required")
		return
	}
	if request.Permissions.DataWrite && !request.Permissions.DataRead {
		writeAPIError(w, http.StatusBadRequest, "invalid_permissions", "data_write requires data_read")
		return
	}
	secret, err := core.NewSecret(32)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "token_generation_failed", err.Error())
		return
	}
	now := time.Now().UTC()
	var sandbox *core.Sandbox
	var operation *core.SandboxOperation
	var sandboxToken string
	err = s.store.Update(func(state *core.State) error {
		lease := state.ResourceLeases[request.ResourceLeaseID]
		if !lease.ActiveAt(now) {
			return fmt.Errorf("resource lease is not active")
		}
		if lease.SandboxID != "" {
			return fmt.Errorf("resource lease already has sandbox %s", lease.SandboxID)
		}
		node := state.Nodes[lease.NodeID]
		if node == nil || !core.ContainsString(node.Capabilities.Capabilities, "sandbox.systemd-bwrap") {
			return fmt.Errorf("selected node does not advertise sandbox.systemd-bwrap")
		}
		sandboxID, idErr := core.NewID("sbx")
		if idErr != nil {
			return idErr
		}
		workspaceID, idErr := core.NewID("vol")
		if idErr != nil {
			return idErr
		}
		homeID, idErr := core.NewID("vol")
		if idErr != nil {
			return idErr
		}
		sandboxToken = sandboxID + "." + secret
		sandbox = &core.Sandbox{
			ID: sandboxID, Name: request.Name, NodeID: lease.NodeID, ResourceLeaseID: lease.ID,
			TokenHash: core.HashSecret(sandboxToken), Status: core.SandboxPending, Permissions: request.Permissions,
			WorkspaceVolumeID: workspaceID, HomeVolumeID: homeID, CreatedAt: now, UpdatedAt: now,
		}
		state.Sandboxes[sandboxID] = sandbox
		state.SandboxVolumes[workspaceID] = &core.SandboxVolume{ID: workspaceID, SandboxID: sandboxID, NodeID: lease.NodeID, Name: "workspace", Kind: "workspace", SizeMB: lease.Resources.DiskMB, MountPath: "/workspace", CreatedAt: now, UpdatedAt: now}
		state.SandboxVolumes[homeID] = &core.SandboxVolume{ID: homeID, SandboxID: sandboxID, NodeID: lease.NodeID, Name: "home", Kind: "home", SizeMB: lease.Resources.DiskMB, MountPath: "/home/agent", CreatedAt: now, UpdatedAt: now}
		lease.SandboxID = sandboxID
		lease.UpdatedAt = now
		operation, idErr = queueSandboxOperation(state, sandbox, "ensure", nil, now)
		if idErr != nil {
			return idErr
		}
		bumpRegistry(state, s.cfg.EventRetention, "sandbox.created", "sandbox", sandboxID, map[string]string{"node_id": lease.NodeID})
		return nil
	})
	if err != nil {
		writeAPIError(w, http.StatusConflict, "sandbox_create_failed", err.Error())
		return
	}
	s.notifyRegistryChanged()
	writeJSON(w, http.StatusCreated, map[string]any{"sandbox": publicSandbox(sandbox), "sandbox_token": sandboxToken, "operation": publicSandboxOperation(operation)})
}

func (s *Server) handleSandboxesList(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	list := []core.Sandbox{}
	_ = s.store.View(func(state *core.State) error {
		for _, sandbox := range state.Sandboxes {
			list = append(list, publicSandbox(sandbox))
		}
		return nil
	})
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt.After(list[j].CreatedAt) })
	writeJSON(w, http.StatusOK, map[string]any{"sandboxes": list, "count": len(list)})
}

func (s *Server) handleSandboxGet(w http.ResponseWriter, r *http.Request) {
	sandboxID := r.PathValue("sandboxID")
	sandbox, ok := s.authenticateSandbox(w, r, sandboxID)
	if !ok {
		return
	}
	volumes := []core.SandboxVolume{}
	_ = s.store.View(func(state *core.State) error {
		for _, volume := range state.SandboxVolumes {
			if volume.SandboxID == sandboxID {
				volumes = append(volumes, cloneViaJSON(*volume))
			}
		}
		return nil
	})
	writeJSON(w, http.StatusOK, map[string]any{"sandbox": publicSandbox(sandbox), "volumes": volumes})
}

func validateSandboxExec(spec *core.SandboxExecSpec) error {
	if len(spec.Command) == 0 {
		return fmt.Errorf("command must contain at least one argument")
	}
	for _, argument := range spec.Command {
		if strings.ContainsRune(argument, 0) {
			return fmt.Errorf("command contains an invalid argument")
		}
	}
	if spec.TimeoutSeconds < 0 {
		return fmt.Errorf("timeout_seconds must be zero or greater")
	}
	if spec.WorkingDir == "" {
		spec.WorkingDir = "/workspace"
	}
	spec.WorkingDir = path.Clean(spec.WorkingDir)
	if spec.WorkingDir != "/workspace" && spec.WorkingDir != "/home/agent" && !strings.HasPrefix(spec.WorkingDir, "/workspace/") && !strings.HasPrefix(spec.WorkingDir, "/home/agent/") {
		return fmt.Errorf("working_dir must be inside /workspace or /home/agent")
	}
	for key, value := range spec.Env {
		if key == "" || strings.ContainsAny(key, "=\x00") || strings.ContainsRune(value, 0) || len(key) > 256 || len(value) > 32768 {
			return fmt.Errorf("environment contains an invalid key or value")
		}
	}
	seenNames := map[string]bool{}
	seenPorts := map[int]bool{}
	for index := range spec.Endpoints {
		endpoint := &spec.Endpoints[index]
		endpoint.Name = strings.TrimSpace(endpoint.Name)
		endpoint.Protocol = strings.ToLower(strings.TrimSpace(endpoint.Protocol))
		endpoint.URL = strings.TrimSpace(endpoint.URL)
		endpoint.HealthURL = strings.TrimSpace(endpoint.HealthURL)
		if endpoint.Name == "" || strings.ContainsRune(endpoint.Name, 0) {
			return fmt.Errorf("endpoint name is required")
		}
		if seenNames[endpoint.Name] {
			return fmt.Errorf("endpoint name %q is duplicated", endpoint.Name)
		}
		seenNames[endpoint.Name] = true
		if endpoint.Protocol != "http" && endpoint.Protocol != "https" && endpoint.Protocol != "tcp" {
			return fmt.Errorf("endpoint protocol must be http, https, or tcp")
		}
		if endpoint.Port < 1 || endpoint.Port > 65535 || seenPorts[endpoint.Port] {
			return fmt.Errorf("endpoint port must be unique and between 1 and 65535")
		}
		seenPorts[endpoint.Port] = true
		parsed, err := url.Parse(endpoint.URL)
		if err != nil || parsed.IsAbs() == false || parsed.Host == "" || parsed.Scheme != endpoint.Protocol {
			return fmt.Errorf("endpoint URL must be an absolute %s URL", endpoint.Protocol)
		}
		urlPort := parsed.Port()
		if urlPort == "" {
			switch endpoint.Protocol {
			case "http":
				urlPort = "80"
			case "https":
				urlPort = "443"
			}
		}
		if urlPort != strconv.Itoa(endpoint.Port) {
			return fmt.Errorf("endpoint URL port must match declared port %d", endpoint.Port)
		}
		if endpoint.HealthURL != "" {
			health, err := url.Parse(endpoint.HealthURL)
			if err != nil || !health.IsAbs() || health.Host == "" || (health.Scheme != "http" && health.Scheme != "https") {
				return fmt.Errorf("endpoint health_url must be an absolute HTTP(S) URL")
			}
			if !strings.EqualFold(health.Host, parsed.Host) {
				return fmt.Errorf("endpoint health_url must use the same host and port as the published URL")
			}
		}
	}
	return nil
}

func normalizeSandboxFilePath(value string) (string, error) {
	cleaned := path.Clean(strings.TrimSpace(value))
	if cleaned == "/workspace" || cleaned == "/home/agent" {
		return "", fmt.Errorf("file path must name a file inside a persistent volume")
	}
	if !strings.HasPrefix(cleaned, "/workspace/") && !strings.HasPrefix(cleaned, "/home/agent/") {
		return "", fmt.Errorf("file path must be inside /workspace or /home/agent")
	}
	if len(cleaned) > 4096 {
		return "", fmt.Errorf("file path exceeds 4096 characters")
	}
	return cleaned, nil
}

func (s *Server) handleSandboxExec(w http.ResponseWriter, r *http.Request) {
	sandboxID := r.PathValue("sandboxID")
	sandboxAuth, ok := s.authenticateSandbox(w, r, sandboxID)
	if !ok {
		return
	}
	if !sandboxAuth.Permissions.Exec {
		writeAPIError(w, http.StatusForbidden, "exec_not_allowed", "sandbox token does not grant exec permission")
		return
	}
	var spec core.SandboxExecSpec
	if !decodeJSON(w, r, s.cfg.MaxRequestBytes, &spec) {
		return
	}
	if err := validateSandboxExec(&spec); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_exec", err.Error())
		return
	}
	now := time.Now().UTC()
	var operation *core.SandboxOperation
	err := s.store.Update(func(state *core.State) error {
		sandbox := state.Sandboxes[sandboxID]
		if sandbox == nil || sandbox.Status != core.SandboxReady {
			return fmt.Errorf("sandbox is not ready")
		}
		lease := state.ResourceLeases[sandbox.ResourceLeaseID]
		if !lease.ActiveAt(now) {
			return fmt.Errorf("sandbox resource lease is not active")
		}
		if len(spec.Endpoints) > 0 && !sandbox.Permissions.Network {
			return fmt.Errorf("published endpoints require sandbox network permission")
		}
		for _, candidate := range spec.Endpoints {
			for _, current := range state.SandboxOperations {
				if current.NodeID != sandbox.NodeID || current.Terminal() || current.Exec == nil {
					continue
				}
				for _, endpoint := range current.Exec.Endpoints {
					if endpoint.Port == candidate.Port {
						return fmt.Errorf("node port %d is already reserved by operation %s", candidate.Port, current.ID)
					}
				}
			}
		}
		for _, current := range state.SandboxOperations {
			if current.SandboxID == sandboxID && !current.Terminal() {
				return fmt.Errorf("sandbox operation %s is still %s", current.ID, current.Status)
			}
		}
		var queueErr error
		operation, queueErr = queueSandboxOperation(state, sandbox, "exec", &spec, now)
		return queueErr
	})
	if err != nil {
		writeAPIError(w, http.StatusConflict, "sandbox_exec_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"operation": publicSandboxOperation(operation)})
}

func (s *Server) handleSandboxFilePush(w http.ResponseWriter, r *http.Request) {
	sandboxID := r.PathValue("sandboxID")
	sandboxAuth, ok := s.authenticateSandbox(w, r, sandboxID)
	if !ok {
		return
	}
	if !sandboxAuth.Permissions.DataWrite {
		writeAPIError(w, http.StatusForbidden, "data_write_not_allowed", "sandbox token does not grant data_write permission")
		return
	}
	ready := false
	_ = s.store.View(func(state *core.State) error {
		sandbox := state.Sandboxes[sandboxID]
		if sandbox != nil && sandbox.Status == core.SandboxReady {
			lease := state.ResourceLeases[sandbox.ResourceLeaseID]
			ready = lease.ActiveAt(time.Now().UTC())
		}
		return nil
	})
	if !ready {
		writeAPIError(w, http.StatusConflict, "sandbox_not_ready", "sandbox and its resource lease must be active before uploading data")
		return
	}
	targetPath, err := normalizeSandboxFilePath(r.URL.Query().Get("path"))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_file_path", err.Error())
		return
	}
	executable := false
	if raw := strings.TrimSpace(r.URL.Query().Get("executable")); raw != "" {
		executable, err = strconv.ParseBool(raw)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid_executable", "executable must be true or false")
			return
		}
	}
	name := r.URL.Query().Get("name")
	if name == "" {
		name = path.Base(targetPath)
	}
	artifact, err := s.ingestArtifact(w, r, name, r.URL.Query().Get("media_type"), "", "", map[string]string{"sandbox_id": sandboxID, "sandbox_path": targetPath})
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "sandbox_file_upload_failed", err.Error())
		return
	}
	now := time.Now().UTC()
	var operation *core.SandboxOperation
	err = s.store.Update(func(state *core.State) error {
		sandbox := state.Sandboxes[sandboxID]
		if sandbox == nil || sandbox.Status != core.SandboxReady {
			return fmt.Errorf("sandbox is not ready")
		}
		lease := state.ResourceLeases[sandbox.ResourceLeaseID]
		if !lease.ActiveAt(now) {
			return fmt.Errorf("sandbox resource lease is not active")
		}
		var queueErr error
		operation, queueErr = queueSandboxFileOperation(state, sandbox, "put", core.SandboxFileSpec{Path: targetPath, ArtifactID: artifact.ID, Executable: executable}, now)
		return queueErr
	})
	if err != nil {
		writeAPIError(w, http.StatusConflict, "sandbox_file_queue_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"artifact": artifact, "operation": publicSandboxOperation(operation)})
}

func (s *Server) handleSandboxFileExport(w http.ResponseWriter, r *http.Request) {
	sandboxID := r.PathValue("sandboxID")
	sandboxAuth, ok := s.authenticateSandbox(w, r, sandboxID)
	if !ok {
		return
	}
	if !sandboxAuth.Permissions.DataRead {
		writeAPIError(w, http.StatusForbidden, "data_read_not_allowed", "sandbox token does not grant data_read permission")
		return
	}
	var request struct {
		Path string `json:"path"`
	}
	if !decodeJSON(w, r, s.cfg.MaxRequestBytes, &request) {
		return
	}
	filePath, err := normalizeSandboxFilePath(request.Path)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_file_path", err.Error())
		return
	}
	now := time.Now().UTC()
	var operation *core.SandboxOperation
	err = s.store.Update(func(state *core.State) error {
		sandbox := state.Sandboxes[sandboxID]
		if sandbox == nil || sandbox.Status != core.SandboxReady {
			return fmt.Errorf("sandbox is not ready")
		}
		lease := state.ResourceLeases[sandbox.ResourceLeaseID]
		if !lease.ActiveAt(now) {
			return fmt.Errorf("sandbox resource lease is not active")
		}
		var queueErr error
		operation, queueErr = queueSandboxFileOperation(state, sandbox, "get", core.SandboxFileSpec{Path: filePath}, now)
		return queueErr
	})
	if err != nil {
		writeAPIError(w, http.StatusConflict, "sandbox_file_export_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"operation": publicSandboxOperation(operation)})
}

func (s *Server) handleSandboxArtifactDownload(w http.ResponseWriter, r *http.Request) {
	sandboxID := r.PathValue("sandboxID")
	sandbox, ok := s.authenticateSandbox(w, r, sandboxID)
	if !ok {
		return
	}
	if !sandbox.Permissions.DataRead {
		writeAPIError(w, http.StatusForbidden, "data_read_not_allowed", "sandbox token does not grant data_read permission")
		return
	}
	artifactID := r.PathValue("artifactID")
	var artifact *core.Artifact
	_ = s.store.View(func(state *core.State) error {
		if current := state.Artifacts[artifactID]; current != nil && current.Metadata["sandbox_id"] == sandboxID {
			copy := cloneViaJSON(*current)
			artifact = &copy
		}
		return nil
	})
	if artifact == nil {
		writeAPIError(w, http.StatusNotFound, "sandbox_artifact_not_found", "artifact is not associated with this sandbox")
		return
	}
	absolutePath, err := artifactAbsolutePath(s.store.ArtifactsDir(), artifact)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "artifact_path_invalid", err.Error())
		return
	}
	file, err := os.Open(absolutePath)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "artifact_content_missing", "artifact content is missing")
		return
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "artifact_stat_failed", err.Error())
		return
	}
	w.Header().Set("Content-Type", artifact.MediaType)
	w.Header().Set("Content-Length", strconv.FormatInt(stat.Size(), 10))
	w.Header().Set("Content-Disposition", contentDisposition(artifact.Name))
	w.Header().Set("ETag", fmt.Sprintf("\"sha256:%s\"", artifact.SHA256))
	http.ServeContent(w, r, filepath.Base(artifact.Name), stat.ModTime(), file)
}

func (s *Server) handleSandboxResize(w http.ResponseWriter, r *http.Request) {
	sandboxID := r.PathValue("sandboxID")
	if _, ok := s.authenticateSandbox(w, r, sandboxID); !ok {
		return
	}
	var request sandboxResizeRequest
	if !decodeJSON(w, r, s.cfg.MaxRequestBytes, &request) {
		return
	}
	if err := validateResourceRequest(request.Resources); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_resources", err.Error())
		return
	}
	now := time.Now().UTC()
	var operation *core.SandboxOperation
	err := s.store.Update(func(state *core.State) error {
		if request.ExpectedRevision != state.RegistryRevision {
			return fmt.Errorf("expected revision %d, current revision is %d", request.ExpectedRevision, state.RegistryRevision)
		}
		sandbox := state.Sandboxes[sandboxID]
		if sandbox == nil || sandbox.Status == core.SandboxDestroyed {
			return fmt.Errorf("sandbox is not active")
		}
		lease := state.ResourceLeases[sandbox.ResourceLeaseID]
		if !lease.ActiveAt(now) {
			return fmt.Errorf("resource lease is not active")
		}
		if request.Resources.DiskMB < lease.Resources.DiskMB {
			return fmt.Errorf("persistent sandbox disk cannot shrink; requested %dMB, current %dMB", request.Resources.DiskMB, lease.Resources.DiskMB)
		}
		node := state.Nodes[sandbox.NodeID]
		if node == nil {
			return fmt.Errorf("node not found")
		}
		if admissionErr := validateAdmission(state, node, request.Resources, now, lease.ID); admissionErr != nil {
			return admissionErr
		}
		var queueErr error
		operation, queueErr = queueSandboxOperation(state, sandbox, "resize", nil, now)
		if queueErr != nil {
			return queueErr
		}
		operation.Resources = &request.Resources
		lease.ComputeReleased = false
		sandbox.Status = core.SandboxPending
		sandbox.UpdatedAt = now
		lease.RegistryRevision = bumpRegistry(state, s.cfg.EventRetention, "sandbox.resized", "sandbox", sandboxID, nil)
		return nil
	})
	if err != nil {
		writeAPIError(w, http.StatusConflict, "sandbox_resize_failed", err.Error())
		return
	}
	s.notifyRegistryChanged()
	writeJSON(w, http.StatusAccepted, map[string]any{"operation": publicSandboxOperation(operation)})
}

func (s *Server) handleSandboxRebind(w http.ResponseWriter, r *http.Request) {
	sandboxID := r.PathValue("sandboxID")
	if _, ok := s.authenticateSandbox(w, r, sandboxID); !ok {
		return
	}
	var request sandboxRebindRequest
	if !decodeJSON(w, r, s.cfg.MaxRequestBytes, &request) {
		return
	}
	request.ResourceLeaseID = strings.TrimSpace(request.ResourceLeaseID)
	if request.ResourceLeaseID == "" {
		writeAPIError(w, http.StatusBadRequest, "invalid_resource_lease_id", "resource_lease_id is required")
		return
	}
	if _, ok := s.authenticateResourceLease(w, r, request.ResourceLeaseID); !ok {
		return
	}
	now := time.Now().UTC()
	var operation *core.SandboxOperation
	err := s.store.Update(func(state *core.State) error {
		sandbox := state.Sandboxes[sandboxID]
		if sandbox == nil || sandbox.Status == core.SandboxDestroyed {
			return fmt.Errorf("sandbox is destroyed")
		}
		oldLease := state.ResourceLeases[sandbox.ResourceLeaseID]
		lease := state.ResourceLeases[request.ResourceLeaseID]
		if !lease.ActiveAt(now) {
			return fmt.Errorf("new resource lease is not active")
		}
		if lease.NodeID != sandbox.NodeID {
			return fmt.Errorf("persistent sandbox can only be rebound on its existing node %s", sandbox.NodeID)
		}
		if lease.SandboxID != "" && lease.SandboxID != sandboxID {
			return fmt.Errorf("new resource lease is already attached to sandbox %s", lease.SandboxID)
		}
		if lease.ComputeReleased {
			node := state.Nodes[sandbox.NodeID]
			if node == nil {
				return fmt.Errorf("sandbox node not found")
			}
			if err := validateAdmission(state, node, lease.Resources, now, lease.ID); err != nil {
				return err
			}
			lease.ComputeReleased = false
		}
		for _, volume := range state.SandboxVolumes {
			if volume.SandboxID == sandboxID && lease.Resources.DiskMB < volume.SizeMB {
				return fmt.Errorf("new resource lease disk cannot shrink persistent volume %s below %dMB", volume.Name, volume.SizeMB)
			}
		}
		if oldLease != nil && oldLease.ID != lease.ID && oldLease.SandboxID == sandboxID {
			oldLease.SandboxID = ""
			oldLease.UpdatedAt = now
		}
		lease.SandboxID = sandboxID
		lease.UpdatedAt = now
		sandbox.ResourceLeaseID = lease.ID
		sandbox.Status = core.SandboxPending
		sandbox.Error = ""
		sandbox.UpdatedAt = now
		var queueErr error
		operation, queueErr = queueSandboxOperation(state, sandbox, "ensure", nil, now)
		if queueErr != nil {
			return queueErr
		}
		for _, volume := range state.SandboxVolumes {
			if volume.SandboxID == sandboxID {
				volume.SizeMB = lease.Resources.DiskMB
				volume.UpdatedAt = now
			}
		}
		revision := bumpRegistry(state, s.cfg.EventRetention, "sandbox.rebound", "sandbox", sandboxID, map[string]string{"resource_lease_id": lease.ID})
		lease.RegistryRevision = revision
		if oldLease != nil && oldLease.ID != lease.ID {
			oldLease.RegistryRevision = revision
		}
		return nil
	})
	if err != nil {
		writeAPIError(w, http.StatusConflict, "sandbox_rebind_failed", err.Error())
		return
	}
	s.notifyRegistryChanged()
	writeJSON(w, http.StatusAccepted, map[string]any{"operation": publicSandboxOperation(operation)})
}

func (s *Server) queueSandboxLifecycle(w http.ResponseWriter, r *http.Request, kind string) {
	sandboxID := r.PathValue("sandboxID")
	if _, ok := s.authenticateSandbox(w, r, sandboxID); !ok {
		return
	}
	now := time.Now().UTC()
	var operation *core.SandboxOperation
	err := s.store.Update(func(state *core.State) error {
		sandbox := state.Sandboxes[sandboxID]
		if sandbox == nil || sandbox.Status == core.SandboxDestroyed {
			return fmt.Errorf("sandbox is not active")
		}
		if kind == "ensure" {
			if sandbox.Status != core.SandboxStopped && sandbox.Status != core.SandboxError {
				return fmt.Errorf("sandbox must be stopped or in error before it can start")
			}
			lease := state.ResourceLeases[sandbox.ResourceLeaseID]
			if !lease.ActiveAt(now) {
				return fmt.Errorf("sandbox resource lease is not active; rebind a new lease")
			}
			node := state.Nodes[sandbox.NodeID]
			if node == nil {
				return fmt.Errorf("sandbox node not found")
			}
			if err := validateAdmission(state, node, lease.Resources, now, lease.ID); err != nil {
				return err
			}
			lease.ComputeReleased = false
		}
		for _, current := range state.SandboxOperations {
			if current.SandboxID != sandboxID || current.Terminal() {
				continue
			}
			if (kind == "stop" || kind == "destroy") && current.Kind == "exec" {
				current.CancelRequested = true
				current.CancelRequestedAt = &now
				current.UpdatedAt = now
				if current.Status == core.SandboxOperationQueued {
					current.Status = core.SandboxOperationCancelled
					current.CancelledAt = &now
					current.FinishedAt = &now
				}
				continue
			}
			return fmt.Errorf("sandbox operation %s is still %s", current.ID, current.Status)
		}
		var queueErr error
		operation, queueErr = queueSandboxOperation(state, sandbox, kind, nil, now)
		if queueErr != nil {
			return queueErr
		}
		sandbox.Status = core.SandboxPending
		sandbox.UpdatedAt = now
		bumpRegistry(state, s.cfg.EventRetention, "sandbox."+kind+"_requested", "sandbox", sandboxID, nil)
		return nil
	})
	if err != nil {
		writeAPIError(w, http.StatusConflict, "sandbox_"+kind+"_failed", err.Error())
		return
	}
	s.notifyRegistryChanged()
	writeJSON(w, http.StatusAccepted, map[string]any{"operation": publicSandboxOperation(operation)})
}

func (s *Server) handleSandboxStop(w http.ResponseWriter, r *http.Request) {
	s.queueSandboxLifecycle(w, r, "stop")
}

func (s *Server) handleSandboxStart(w http.ResponseWriter, r *http.Request) {
	s.queueSandboxLifecycle(w, r, "ensure")
}

func (s *Server) handleSandboxDestroy(w http.ResponseWriter, r *http.Request) {
	s.queueSandboxLifecycle(w, r, "destroy")
}

func (s *Server) handleSandboxOperationGet(w http.ResponseWriter, r *http.Request) {
	operationID := r.PathValue("operationID")
	var operation *core.SandboxOperation
	_ = s.store.View(func(state *core.State) error {
		if current := state.SandboxOperations[operationID]; current != nil {
			copy := cloneViaJSON(*current)
			operation = &copy
		}
		return nil
	})
	if operation == nil {
		writeAPIError(w, http.StatusNotFound, "sandbox_operation_not_found", "sandbox operation not found")
		return
	}
	if _, ok := s.authenticateSandbox(w, r, operation.SandboxID); !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"operation": publicSandboxOperation(operation)})
}

func (s *Server) handleSandboxOperationCancel(w http.ResponseWriter, r *http.Request) {
	operationID := r.PathValue("operationID")
	var sandboxID string
	_ = s.store.View(func(state *core.State) error {
		if operation := state.SandboxOperations[operationID]; operation != nil {
			sandboxID = operation.SandboxID
		}
		return nil
	})
	if sandboxID == "" {
		writeAPIError(w, http.StatusNotFound, "sandbox_operation_not_found", "sandbox operation not found")
		return
	}
	if _, ok := s.authenticateSandbox(w, r, sandboxID); !ok {
		return
	}
	now := time.Now().UTC()
	var response core.SandboxOperation
	err := s.store.Update(func(state *core.State) error {
		operation := state.SandboxOperations[operationID]
		if operation == nil {
			return fmt.Errorf("sandbox operation not found")
		}
		if !operation.Terminal() {
			operation.CancelRequested = true
			operation.CancelRequestedAt = &now
			operation.UpdatedAt = now
			if operation.Status == core.SandboxOperationQueued {
				operation.Status = core.SandboxOperationCancelled
				operation.CancelledAt = &now
				operation.FinishedAt = &now
			}
		}
		response = publicSandboxOperation(operation)
		return nil
	})
	if err != nil {
		writeAPIError(w, http.StatusConflict, "sandbox_operation_cancel_failed", err.Error())
		return
	}
	s.notifyRegistryChanged()
	writeJSON(w, http.StatusOK, map[string]any{"operation": response})
}

func (s *Server) handleSandboxOperationClaim(w http.ResponseWriter, r *http.Request) {
	nodeID := r.PathValue("nodeID")
	if _, ok := s.authenticateNode(w, r, nodeID); !ok {
		return
	}
	var empty struct{}
	if !decodeJSON(w, r, s.cfg.MaxRequestBytes, &empty) {
		return
	}
	queued := false
	_ = s.store.View(func(state *core.State) error {
		for _, operation := range state.SandboxOperations {
			if operation.NodeID == nodeID && operation.Status == core.SandboxOperationQueued {
				queued = true
				break
			}
		}
		return nil
	})
	if !queued {
		writeJSON(w, http.StatusOK, sandboxOperationClaimResponse{})
		return
	}
	secret, err := core.NewSecret(32)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "token_generation_failed", err.Error())
		return
	}
	now := time.Now().UTC()
	response := sandboxOperationClaimResponse{}
	err = s.store.Update(func(state *core.State) error {
		var selected *core.SandboxOperation
		for _, operation := range state.SandboxOperations {
			if operation.NodeID != nodeID || operation.Status != core.SandboxOperationQueued {
				continue
			}
			node := state.Nodes[nodeID]
			if node == nil || (node.Status != core.NodeOnline && operation.Kind != "stop" && operation.Kind != "destroy") {
				continue
			}
			busy := false
			for _, current := range state.SandboxOperations {
				if current.SandboxID == operation.SandboxID && !current.Terminal() && current.Status != core.SandboxOperationQueued {
					busy = true
					break
				}
			}
			if busy {
				continue
			}
			if selected == nil || operation.CreatedAt.Before(selected.CreatedAt) || (operation.CreatedAt.Equal(selected.CreatedAt) && operation.ID < selected.ID) {
				selected = operation
			}
		}
		if selected == nil {
			return nil
		}
		sandbox := state.Sandboxes[selected.SandboxID]
		if sandbox == nil {
			selected.Status = core.SandboxOperationFailed
			selected.Error = "sandbox no longer exists"
			selected.UpdatedAt = now
			selected.FinishedAt = &now
			return nil
		}
		if sandbox.Status == core.SandboxDestroyed && selected.Kind != "destroy" {
			selected.Status = core.SandboxOperationFailed
			selected.Error = "sandbox is destroyed"
			selected.UpdatedAt = now
			selected.FinishedAt = &now
			return nil
		}
		lease := state.ResourceLeases[sandbox.ResourceLeaseID]
		if selected.Kind != "stop" && selected.Kind != "destroy" && !lease.ActiveAt(now) {
			selected.Status = core.SandboxOperationFailed
			selected.Error = "resource lease is not active"
			selected.UpdatedAt = now
			selected.FinishedAt = &now
			return nil
		}
		operationToken := selected.ID + "." + secret
		selected.Status = core.SandboxOperationClaimed
		selected.ClaimTokenHash = core.HashSecret(operationToken)
		selected.ClaimedAt = &now
		selected.UpdatedAt = now
		operationCopy := publicSandboxOperation(selected)
		sandboxCopy := publicSandbox(sandbox)
		response = sandboxOperationClaimResponse{Claimed: true, Operation: &operationCopy, OperationToken: operationToken, Sandbox: &sandboxCopy}
		if lease != nil {
			resources := lease.Resources
			if selected.Resources != nil {
				resources = *selected.Resources
			}
			response.Resources = &resources
			response.ResourceLeaseExpires = lease.ExpiresAt
		}
		return nil
	})
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "sandbox_operation_claim_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, response)
}

type sandboxOperationReconcileRequest struct {
	Running     bool   `json:"running"`
	ActiveState string `json:"active_state,omitempty"`
	SubState    string `json:"sub_state,omitempty"`
}

func (s *Server) handleSandboxOperationReconcile(w http.ResponseWriter, r *http.Request) {
	nodeID := r.PathValue("nodeID")
	if _, ok := s.authenticateNode(w, r, nodeID); !ok {
		return
	}
	var request sandboxOperationReconcileRequest
	if !decodeJSON(w, r, s.cfg.MaxRequestBytes, &request) {
		return
	}
	operationID := r.PathValue("operationID")
	now := time.Now().UTC()
	changed := false
	var response core.SandboxOperation
	err := s.store.Update(func(state *core.State) error {
		operation := state.SandboxOperations[operationID]
		if operation == nil || operation.NodeID != nodeID {
			return fmt.Errorf("sandbox operation not found for node")
		}
		if operation.Status != core.SandboxOperationOrphaned {
			response = publicSandboxOperation(operation)
			return nil
		}
		changed = true
		operation.UpdatedAt = now
		if request.Running {
			operation.Status = core.SandboxOperationRunning
			operation.Error = ""
		} else {
			operation.Status = core.SandboxOperationFailed
			operation.Error = "worker could not find a running sandbox service unit after control-plane recovery"
			if request.ActiveState != "" || request.SubState != "" {
				operation.Error += fmt.Sprintf(" (active=%s, sub=%s)", request.ActiveState, request.SubState)
			}
			operation.FinishedAt = &now
			operation.ClaimTokenHash = ""
			if sandbox := state.Sandboxes[operation.SandboxID]; sandbox != nil && sandbox.ServiceOperationID == operation.ID {
				sandbox.ServiceOperationID = ""
				sandbox.PublishedEndpoints = nil
				sandbox.UpdatedAt = now
			}
		}
		bumpRegistry(state, s.cfg.EventRetention, "sandbox.operation_reconciled", "sandbox", operation.SandboxID, map[string]string{"operation_id": operation.ID, "running": fmt.Sprint(request.Running)})
		response = publicSandboxOperation(operation)
		return nil
	})
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "sandbox_operation_reconcile_failed", err.Error())
		return
	}
	if changed {
		s.notifyRegistryChanged()
	}
	writeJSON(w, http.StatusOK, map[string]any{"operation": response})
}

func (s *Server) handleSandboxOperationStart(w http.ResponseWriter, r *http.Request) {
	operationID := r.PathValue("operationID")
	var operationNodeID string
	_ = s.store.View(func(state *core.State) error {
		if operation := state.SandboxOperations[operationID]; operation != nil {
			operationNodeID = operation.NodeID
		}
		return nil
	})
	if operationNodeID == "" {
		writeAPIError(w, http.StatusNotFound, "sandbox_operation_not_found", "sandbox operation not found")
		return
	}
	if _, ok := s.authenticateNode(w, r, operationNodeID); !ok {
		return
	}
	operationToken := strings.TrimSpace(r.Header.Get("X-AgentsPool-Sandbox-Operation-Token"))
	if operationToken == "" {
		writeAPIError(w, http.StatusUnauthorized, "operation_token_required", "sandbox operation token is required")
		return
	}
	now := time.Now().UTC()
	var response core.SandboxOperation
	err := s.store.Update(func(state *core.State) error {
		operation := state.SandboxOperations[operationID]
		if operation == nil || (operation.Status != core.SandboxOperationClaimed && operation.Status != core.SandboxOperationOrphaned && operation.Status != core.SandboxOperationRunning) {
			return fmt.Errorf("sandbox operation is not claimable")
		}
		if !core.SecureHashEqual(operation.ClaimTokenHash, operationToken) {
			return fmt.Errorf("invalid sandbox operation token")
		}
		operation.Status = core.SandboxOperationRunning
		operation.Error = ""
		operation.UpdatedAt = now
		if operation.StartedAt == nil {
			operation.StartedAt = &now
		}
		if operation.Exec != nil && len(operation.Exec.Endpoints) > 0 {
			if sandbox := state.Sandboxes[operation.SandboxID]; sandbox != nil {
				sandbox.ServiceOperationID = operation.ID
				sandbox.PublishedEndpoints = cloneViaJSON(operation.Exec.Endpoints)
				sandbox.UpdatedAt = now
				bumpRegistry(state, s.cfg.EventRetention, "sandbox.endpoints_published", "sandbox", sandbox.ID, map[string]string{"operation_id": operation.ID})
			}
		}
		response = publicSandboxOperation(operation)
		return nil
	})
	if err != nil {
		writeAPIError(w, http.StatusConflict, "sandbox_operation_start_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"operation": response})
}

// A Worker refreshes this policy while executing, including immediately before
// enforcing an old deadline. The scoped operation credential is never exposed
// to the sandbox itself.
func (s *Server) handleSandboxOperationLease(w http.ResponseWriter, r *http.Request) {
	nodeID := r.PathValue("nodeID")
	if _, ok := s.authenticateNode(w, r, nodeID); !ok {
		return
	}
	operationID := r.PathValue("operationID")
	token := strings.TrimSpace(r.Header.Get("X-AgentsPool-Sandbox-Operation-Token"))
	var policy core.SandboxLeasePolicy
	err := s.store.View(func(state *core.State) error {
		operation := state.SandboxOperations[operationID]
		if operation == nil || operation.NodeID != nodeID || operation.Terminal() || token == "" || !core.SecureHashEqual(operation.ClaimTokenHash, token) {
			return fmt.Errorf("operation claim is no longer active")
		}
		sandbox := state.Sandboxes[operation.SandboxID]
		if sandbox == nil {
			return fmt.Errorf("sandbox no longer exists")
		}
		lease := state.ResourceLeases[sandbox.ResourceLeaseID]
		policy.Active = lease.ActiveAt(time.Now().UTC()) && !lease.ComputeReleased
		if lease != nil && lease.ExpiresAt != nil {
			expires := *lease.ExpiresAt
			policy.ExpiresAt = &expires
		}
		return nil
	})
	if err != nil {
		writeAPIError(w, http.StatusConflict, "operation_policy_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, policy)
}

func (s *Server) handleSandboxOperationCancelledByNode(w http.ResponseWriter, r *http.Request) {
	operationID := r.PathValue("operationID")
	var nodeID string
	_ = s.store.View(func(state *core.State) error {
		if operation := state.SandboxOperations[operationID]; operation != nil {
			nodeID = operation.NodeID
		}
		return nil
	})
	if nodeID == "" {
		writeAPIError(w, http.StatusNotFound, "sandbox_operation_not_found", "sandbox operation not found")
		return
	}
	if _, ok := s.authenticateNode(w, r, nodeID); !ok {
		return
	}
	now := time.Now().UTC()
	var response core.SandboxOperation
	err := s.store.Update(func(state *core.State) error {
		operation := state.SandboxOperations[operationID]
		if operation == nil || operation.Terminal() {
			return fmt.Errorf("sandbox operation is not active")
		}
		if !operation.CancelRequested {
			return fmt.Errorf("sandbox operation cancellation was not requested")
		}
		operation.Status = core.SandboxOperationCancelled
		operation.CancelledAt = &now
		operation.FinishedAt = &now
		operation.UpdatedAt = now
		operation.ClaimTokenHash = ""
		if sandbox := state.Sandboxes[operation.SandboxID]; sandbox != nil && sandbox.ServiceOperationID == operation.ID {
			sandbox.ServiceOperationID = ""
			sandbox.PublishedEndpoints = nil
			sandbox.UpdatedAt = now
			bumpRegistry(state, s.cfg.EventRetention, "sandbox.endpoints_withdrawn", "sandbox", sandbox.ID, map[string]string{"operation_id": operation.ID})
		}
		response = publicSandboxOperation(operation)
		return nil
	})
	if err != nil {
		writeAPIError(w, http.StatusConflict, "sandbox_operation_cancel_reconcile_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"operation": response})
}

func (s *Server) handleSandboxOperationComplete(w http.ResponseWriter, r *http.Request) {
	operationID := r.PathValue("operationID")
	var operationNodeID string
	destroyOperation := false
	_ = s.store.View(func(state *core.State) error {
		if operation := state.SandboxOperations[operationID]; operation != nil {
			operationNodeID = operation.NodeID
			destroyOperation = operation.Kind == "destroy"
		}
		return nil
	})
	if operationNodeID == "" {
		writeAPIError(w, http.StatusNotFound, "sandbox_operation_not_found", "sandbox operation not found")
		return
	}
	if _, ok := s.authenticateNode(w, r, operationNodeID); !ok {
		return
	}
	operationToken := strings.TrimSpace(r.Header.Get("X-AgentsPool-Sandbox-Operation-Token"))
	if operationToken == "" {
		writeAPIError(w, http.StatusUnauthorized, "operation_token_required", "sandbox operation token is required")
		return
	}
	var request sandboxOperationCompleteRequest
	if !decodeJSON(w, r, s.cfg.MaxRequestBytes, &request) {
		return
	}
	if destroyOperation {
		s.artifactMu.Lock()
		defer s.artifactMu.Unlock()
	}
	now := time.Now().UTC()
	var response core.SandboxOperation
	removedArtifacts := []core.Artifact{}
	removeContent := map[string]bool{}
	err := s.store.Update(func(state *core.State) error {
		operation := state.SandboxOperations[operationID]
		if operation == nil || (operation.Status != core.SandboxOperationClaimed && operation.Status != core.SandboxOperationRunning && operation.Status != core.SandboxOperationOrphaned) {
			return fmt.Errorf("sandbox operation is not active")
		}
		if !core.SecureHashEqual(operation.ClaimTokenHash, operationToken) {
			return fmt.Errorf("invalid sandbox operation token")
		}
		operation.ExitCode = request.ExitCode
		operation.Stdout = request.Stdout
		operation.Stderr = request.Stderr
		operation.OutputTruncated = request.OutputTruncated
		operation.Error = request.Error
		operation.UpdatedAt = now
		operation.FinishedAt = &now
		operation.ClaimTokenHash = ""
		if request.Success && operation.Kind == "get" && operation.ResultArtifactID == "" {
			return fmt.Errorf("file export cannot succeed before its artifact is uploaded")
		}
		if request.Cancelled || operation.CancelRequested {
			operation.Status = core.SandboxOperationCancelled
			operation.CancelledAt = &now
		} else if request.Success {
			operation.Status = core.SandboxOperationSucceeded
		} else {
			operation.Status = core.SandboxOperationFailed
		}
		sandbox := state.Sandboxes[operation.SandboxID]
		if sandbox != nil {
			lease := state.ResourceLeases[sandbox.ResourceLeaseID]
			if operation.Kind == "resize" && operation.Resources != nil && lease != nil {
				if request.Success {
					lease.Resources = *operation.Resources
				} else {
					// A failed grow may already have enlarged the disk image.
					// Reserve that high-water mark until a successful recovery.
					lease.Resources.DiskMB = max64(lease.Resources.DiskMB, operation.Resources.DiskMB)
				}
				lease.UpdatedAt = now
				for _, volume := range state.SandboxVolumes {
					if volume.SandboxID == sandbox.ID {
						volume.SizeMB = lease.Resources.DiskMB
						volume.UpdatedAt = now
					}
				}
			}
			if sandbox.ServiceOperationID == operation.ID {
				sandbox.ServiceOperationID = ""
				sandbox.PublishedEndpoints = nil
			}
			sandbox.UpdatedAt = now
			if request.Success {
				switch operation.Kind {
				case "ensure", "resize":
					sandbox.Status = core.SandboxReady
					sandbox.Error = ""
					if lease != nil {
						lease.ComputeReleased = false
					}
				case "exec":
					sandbox.LastUsedAt = &now
				case "stop":
					sandbox.Status = core.SandboxStopped
					if lease != nil {
						lease.ComputeReleased = true
						lease.UpdatedAt = now
					}
				case "destroy":
					sandbox.Status = core.SandboxDestroyed
					removedArtifacts = purgeSandboxArtifactsLocked(state, sandbox.ID)
					for _, artifact := range removedArtifacts {
						removeContent[artifact.StoragePath] = artifactContentUnreferenced(state, artifact.StoragePath)
					}
					if len(removedArtifacts) > 0 {
						addEvent(state, s.cfg.EventRetention, "sandbox.artifacts_deleted", "node:"+operation.NodeID, "sandbox", sandbox.ID, "destroyed sandbox bridge artifacts", map[string]string{"count": fmt.Sprint(len(removedArtifacts))})
					}
				}
			} else if operation.Kind != "exec" {
				sandbox.Status = core.SandboxError
				sandbox.Error = request.Error
			}
			bumpRegistry(state, s.cfg.EventRetention, "sandbox.operation_completed", "sandbox", sandbox.ID, map[string]string{"kind": operation.Kind, "success": fmt.Sprint(request.Success)})
		}
		response = publicSandboxOperation(operation)
		return nil
	})
	if err != nil {
		writeAPIError(w, http.StatusConflict, "sandbox_operation_complete_failed", err.Error())
		return
	}
	removedPaths := map[string]bool{}
	for _, artifact := range removedArtifacts {
		if !removeContent[artifact.StoragePath] || removedPaths[artifact.StoragePath] {
			continue
		}
		removedPaths[artifact.StoragePath] = true
		warnings, _ := s.removeArtifactContentIfUnreferenced(artifact, true)
		for _, warning := range warnings {
			s.logger.Warn("sandbox destroy artifact cleanup warning", "operation_id", operationID, "warning", warning)
		}
	}
	s.notifyRegistryChanged()
	writeJSON(w, http.StatusOK, map[string]any{"operation": response})
}

func (s *Server) handleSandboxOperationArtifactUpload(w http.ResponseWriter, r *http.Request) {
	operationID := r.PathValue("operationID")
	operationToken := strings.TrimSpace(r.Header.Get("X-AgentsPool-Sandbox-Operation-Token"))
	if operationToken == "" {
		writeAPIError(w, http.StatusUnauthorized, "operation_token_required", "sandbox operation token is required")
		return
	}
	var operation *core.SandboxOperation
	_ = s.store.View(func(state *core.State) error {
		if current := state.SandboxOperations[operationID]; current != nil {
			copy := cloneViaJSON(*current)
			operation = &copy
		}
		return nil
	})
	if operation == nil {
		writeAPIError(w, http.StatusNotFound, "sandbox_operation_not_found", "sandbox operation not found")
		return
	}
	if _, ok := s.authenticateNode(w, r, operation.NodeID); !ok {
		return
	}
	if (operation.Status != core.SandboxOperationClaimed && operation.Status != core.SandboxOperationRunning && operation.Status != core.SandboxOperationOrphaned) || operation.Kind != "get" || operation.File == nil || !core.SecureHashEqual(operation.ClaimTokenHash, operationToken) {
		writeAPIError(w, http.StatusConflict, "sandbox_operation_not_exporting", "sandbox operation is not an authenticated file export")
		return
	}
	name := path.Base(operation.File.Path)
	artifact, err := s.ingestArtifact(w, r, name, r.URL.Query().Get("media_type"), "", "", map[string]string{"sandbox_id": operation.SandboxID, "sandbox_path": operation.File.Path, "sandbox_operation_id": operation.ID})
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "sandbox_artifact_upload_failed", err.Error())
		return
	}
	err = s.store.Update(func(state *core.State) error {
		current := state.SandboxOperations[operationID]
		if current == nil || (current.Status != core.SandboxOperationClaimed && current.Status != core.SandboxOperationRunning && current.Status != core.SandboxOperationOrphaned) || !core.SecureHashEqual(current.ClaimTokenHash, operationToken) {
			return fmt.Errorf("sandbox operation claim changed during upload")
		}
		current.ResultArtifactID = artifact.ID
		current.UpdatedAt = time.Now().UTC()
		return nil
	})
	if err != nil {
		writeAPIError(w, http.StatusConflict, "sandbox_artifact_attach_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"artifact": artifact})
}
