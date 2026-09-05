package server

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/qiyuehuajin/agentspool/internal/core"
)

type workerRegisterRequest struct {
	MachineID    string                `json:"machine_id"`
	Name         string                `json:"name"`
	Capabilities core.NodeCapabilities `json:"capabilities"`
	Resources    core.NodeResources    `json:"resources"`
}

type workerRegisterResponse struct {
	Node      map[string]any `json:"node"`
	NodeToken string         `json:"node_token"`
}

type workerHeartbeatRequest struct {
	Capabilities *core.NodeCapabilities `json:"capabilities,omitempty"`
	Resources    core.NodeResources     `json:"resources"`
}

func (s *Server) handleWorkerRegister(w http.ResponseWriter, r *http.Request) {
	joinToken := strings.TrimSpace(r.Header.Get("X-AgentsPool-Join-Token"))
	if joinToken == "" || !core.SecureHashEqual(core.HashSecret(s.cfg.JoinToken), joinToken) {
		s.metrics.AuthenticationFails.Add(1)
		writeAPIError(w, http.StatusUnauthorized, "invalid_join_token", "a valid join token is required")
		return
	}
	var request workerRegisterRequest
	if !decodeJSON(w, r, s.cfg.MaxRequestBytes, &request) {
		return
	}
	request.MachineID = strings.TrimSpace(request.MachineID)
	request.Name = strings.TrimSpace(request.Name)
	if request.MachineID == "" || len(request.MachineID) > 256 {
		writeAPIError(w, http.StatusBadRequest, "invalid_machine_id", "machine_id is required and must not exceed 256 characters")
		return
	}
	if request.Name == "" || len(request.Name) > 200 {
		writeAPIError(w, http.StatusBadRequest, "invalid_name", "name is required and must not exceed 200 characters")
		return
	}
	if request.Capabilities.CPUThreads < 1 || request.Capabilities.MemoryTotalMB < 1 {
		writeAPIError(w, http.StatusBadRequest, "invalid_capabilities", "CPU and memory capabilities are required")
		return
	}
	if request.Resources.MaxConcurrency < 1 {
		request.Resources.MaxConcurrency = 1
	}
	now := time.Now().UTC()
	secret, err := core.NewSecret(32)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "token_generation_failed", err.Error())
		return
	}
	var registered *core.Node
	var nodeToken string
	err = s.store.Update(func(state *core.State) error {
		var node *core.Node
		for _, existing := range state.Nodes {
			if existing.MachineID == request.MachineID {
				node = existing
				break
			}
		}
		if node == nil {
			nodeID, idErr := core.NewID("node")
			if idErr != nil {
				return idErr
			}
			node = &core.Node{ID: nodeID, MachineID: request.MachineID, RegisteredAt: now}
			state.Nodes[nodeID] = node
		}
		nodeToken = node.ID + "." + secret
		node.Name = request.Name
		node.TokenHash = core.HashSecret(nodeToken)
		node.Status = core.NodeOnline
		node.Capabilities = request.Capabilities
		node.Resources = request.Resources
		node.Resources.ActiveJobs = activeLeaseCountForNode(state, node.ID, now)
		node.Resources.ObservedAt = now
		node.LastSeenAt = now
		node.UpdatedAt = now
		node.DisabledAt = nil
		copy := cloneViaJSON(*node)
		registered = &copy
		addEvent(state, s.cfg.EventRetention, "node.registered", "join-token", "node", node.ID, "worker registered and credentials rotated", map[string]string{"name": node.Name, "machine_id": node.MachineID})
		bumpRegistry(state, s.cfg.EventRetention, "device.registered", "device", node.ID, map[string]string{"name": node.Name})
		return nil
	})
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "registration_failed", err.Error())
		return
	}
	s.notifyRegistryChanged()
	writeJSON(w, http.StatusCreated, workerRegisterResponse{Node: publicNode(registered), NodeToken: nodeToken})
}

func (s *Server) handleWorkerHeartbeat(w http.ResponseWriter, r *http.Request) {
	nodeID := r.PathValue("nodeID")
	if _, ok := s.authenticateNode(w, r, nodeID); !ok {
		return
	}
	var request workerHeartbeatRequest
	if !decodeJSON(w, r, s.cfg.MaxRequestBytes, &request) {
		return
	}
	now := time.Now().UTC()
	var response map[string]any
	registryChanged := false
	err := s.store.UpdateVolatile(func(state *core.State) error {
		node := state.Nodes[nodeID]
		if node == nil {
			return fmt.Errorf("node not found")
		}
		if request.Capabilities != nil {
			node.Capabilities = *request.Capabilities
		}
		request.Resources.ObservedAt = now
		request.Resources.ActiveJobs = activeLeaseCountForNode(state, nodeID, now)
		node.Resources = request.Resources
		node.LastSeenAt = now
		node.UpdatedAt = now
		if node.Status == core.NodeOffline {
			node.Status = core.NodeOnline
			addEvent(state, s.cfg.EventRetention, "node.online", "node:"+nodeID, "node", nodeID, "node resumed heartbeats", nil)
			bumpRegistry(state, s.cfg.EventRetention, "device.online", "device", nodeID, nil)
			registryChanged = true
		}
		cancelLeases := []string{}
		for _, lease := range state.Leases {
			if lease.NodeID == nodeID && lease.Status == core.LeaseActive {
				if task := state.Tasks[lease.TaskID]; task != nil && task.CancelRequested {
					cancelLeases = append(cancelLeases, lease.ID)
				}
			}
		}
		cancelSandboxOperations := []string{}
		reconcileSandboxOperations := []string{}
		for _, operation := range state.SandboxOperations {
			if operation.NodeID != nodeID {
				continue
			}
			if operation.CancelRequested && !operation.Terminal() {
				cancelSandboxOperations = append(cancelSandboxOperations, operation.ID)
			}
			if operation.Status == core.SandboxOperationOrphaned {
				reconcileSandboxOperations = append(reconcileSandboxOperations, operation.ID)
			}
		}
		response = map[string]any{
			"server_time": now, "node_status": node.Status,
			"cancel_lease_ids":                cancelLeases,
			"cancel_sandbox_operation_ids":    cancelSandboxOperations,
			"reconcile_sandbox_operation_ids": reconcileSandboxOperations,
		}
		return nil
	})
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "node_not_found", err.Error())
		return
	}
	if registryChanged {
		s.notifyRegistryChanged()
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleWorkerClaim(w http.ResponseWriter, r *http.Request) {
	nodeID := r.PathValue("nodeID")
	if _, ok := s.authenticateNode(w, r, nodeID); !ok {
		return
	}
	var request ClaimRequest
	if !decodeJSON(w, r, s.cfg.MaxRequestBytes, &request) {
		return
	}
	if len(request.CachedArtifactIDs) > 10000 {
		writeAPIError(w, http.StatusBadRequest, "too_many_cached_artifacts", "cached_artifact_ids exceeds 10000 entries")
		return
	}
	now := time.Now().UTC()
	claimable := false
	err := s.store.View(func(state *core.State) error {
		node := state.Nodes[nodeID]
		if node == nil {
			return fmt.Errorf("node not found")
		}
		observed := cloneViaJSON(*node)
		observed.Resources = request.Resources
		observed.Resources.ActiveJobs = activeLeaseCountForNode(state, nodeID, now)
		if observed.Status == core.NodeOnline && (observed.Resources.MaxConcurrency <= 0 || observed.Resources.ActiveJobs < observed.Resources.MaxConcurrency) {
			claimable = selectTask(state, &observed, request.CachedArtifactIDs, now) != nil
		}
		return nil
	})
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "node_not_found", err.Error())
		return
	}
	if !claimable {
		writeJSON(w, http.StatusOK, ClaimResponse{Claimed: false})
		return
	}
	leaseSecret, err := core.NewSecret(32)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "token_generation_failed", err.Error())
		return
	}
	response := ClaimResponse{Claimed: false}
	err = s.store.Update(func(state *core.State) error {
		node := state.Nodes[nodeID]
		if node == nil {
			return fmt.Errorf("node not found")
		}
		node.Resources = request.Resources
		node.Resources.ActiveJobs = activeLeaseCountForNode(state, nodeID, now)
		node.Resources.ObservedAt = now
		node.LastSeenAt = now
		node.UpdatedAt = now
		if node.Status == core.NodeOffline {
			node.Status = core.NodeOnline
		}
		if node.Status != core.NodeOnline {
			return nil
		}
		if node.Resources.MaxConcurrency > 0 && node.Resources.ActiveJobs >= node.Resources.MaxConcurrency {
			return nil
		}
		task := selectTask(state, node, request.CachedArtifactIDs, now)
		if task == nil {
			return nil
		}
		leaseID, idErr := core.NewID("lease")
		if idErr != nil {
			return idErr
		}
		executionID, idErr := core.NewID("exec")
		if idErr != nil {
			return idErr
		}
		leaseToken := leaseID + "." + leaseSecret
		expires := now.Add(s.cfg.LeaseTTL)
		task.Status = core.TaskLeased
		task.Attempts++
		task.AssignedNodeID = nodeID
		task.ActiveLeaseID = leaseID
		task.UpdatedAt = now
		task.NextEligibleAt = nil
		task.FinishedAt = nil
		task.LastError = ""
		task.Result = nil
		lease := &core.Lease{
			ID: leaseID, TaskID: task.ID, NodeID: nodeID, ExecutionID: executionID,
			TokenHash: core.HashSecret(leaseToken), Status: core.LeaseActive,
			TTLSeconds: int(s.cfg.LeaseTTL.Seconds()), CreatedAt: now, LastRenewedAt: now, ExpiresAt: expires,
		}
		execution := &core.Execution{
			ID: executionID, TaskID: task.ID, NodeID: nodeID, LeaseID: leaseID,
			Attempt: task.Attempts, Status: core.ExecutionLeased, CreatedAt: now,
		}
		state.Leases[leaseID] = lease
		state.Executions[executionID] = execution
		syncNodeActiveJobsLocked(state, nodeID, now)
		taskCopy := cloneViaJSON(*task)
		response = ClaimResponse{
			Claimed: true, Task: &taskCopy, LeaseID: leaseID, LeaseToken: leaseToken,
			ExecutionID: executionID, LeaseExpiresAt: &expires, LeaseTTLSeconds: lease.TTLSeconds,
		}
		addEvent(state, s.cfg.EventRetention, "lease.claimed", "node:"+nodeID, "task", task.ID, "task leased to worker", map[string]string{"lease_id": leaseID, "execution_id": executionID, "attempt": fmt.Sprint(task.Attempts)})
		bumpRegistry(state, s.cfg.EventRetention, "device.workload_changed", "device", nodeID, map[string]string{"task_id": task.ID})
		return nil
	})
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "claim_failed", err.Error())
		return
	}
	if response.Claimed {
		s.metrics.ClaimsTotal.Add(1)
	}
	if response.Claimed {
		s.notifyRegistryChanged()
	}
	writeJSON(w, http.StatusOK, response)
}
