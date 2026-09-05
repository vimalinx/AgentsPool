package server

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/qiyuehuajin/agentspool/internal/config"
	"github.com/qiyuehuajin/agentspool/internal/core"
)

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "version": config.Version})
}

func (s *Server) handleReady(w http.ResponseWriter, _ *http.Request) {
	if _, err := s.store.Snapshot(); err != nil {
		writeAPIError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ready", "version": config.Version})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	state, err := s.store.Snapshot()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "snapshot_failed", err.Error())
		return
	}
	nodeCounts := map[string]int{}
	taskCounts := map[string]int{}
	for _, node := range state.Nodes {
		nodeCounts[node.Status]++
	}
	for _, task := range state.Tasks {
		taskCounts[task.Status]++
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version":        config.Version,
		"started_at":     s.startedAt,
		"uptime_seconds": int(time.Since(s.startedAt).Seconds()),
		"node_counts":    nodeCounts,
		"task_counts":    taskCounts,
		"artifacts":      len(state.Artifacts),
		"executions":     len(state.Executions),
	})
}

func (s *Server) handleNodesList(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	statusFilter := strings.TrimSpace(r.URL.Query().Get("status"))
	nodes := []map[string]any{}
	_ = s.store.View(func(state *core.State) error {
		for _, node := range state.Nodes {
			if statusFilter != "" && node.Status != statusFilter {
				continue
			}
			nodes = append(nodes, publicNode(node))
		}
		return nil
	})
	sort.Slice(nodes, func(i, j int) bool {
		return fmt.Sprint(nodes[i]["name"]) < fmt.Sprint(nodes[j]["name"])
	})
	writeJSON(w, http.StatusOK, map[string]any{"nodes": nodes, "count": len(nodes)})
}

func (s *Server) handleNodeGet(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	nodeID := r.PathValue("nodeID")
	var nodeMap map[string]any
	var activeTasks []*core.Task
	_ = s.store.View(func(state *core.State) error {
		if node := state.Nodes[nodeID]; node != nil {
			nodeMap = publicNode(node)
			for _, task := range state.Tasks {
				if task.AssignedNodeID == nodeID && (task.Status == core.TaskLeased || task.Status == core.TaskRunning) {
					copy := cloneViaJSON(*task)
					activeTasks = append(activeTasks, &copy)
				}
			}
		}
		return nil
	})
	if nodeMap == nil {
		writeAPIError(w, http.StatusNotFound, "node_not_found", "node not found")
		return
	}
	nodeMap["active_tasks"] = activeTasks
	writeJSON(w, http.StatusOK, map[string]any{"node": nodeMap})
}

func (s *Server) handleNodeDrain(w http.ResponseWriter, r *http.Request) {
	s.setNodeStatus(w, r, core.NodeDraining, false)
}

func (s *Server) handleNodeEnable(w http.ResponseWriter, r *http.Request) {
	s.setNodeStatus(w, r, core.NodeOffline, false)
}

func (s *Server) handleNodeDisable(w http.ResponseWriter, r *http.Request) {
	s.setNodeStatus(w, r, core.NodeDisabled, true)
}

func (s *Server) setNodeStatus(w http.ResponseWriter, r *http.Request, target string, expireLeases bool) {
	if !s.requireAdmin(w, r) {
		return
	}
	nodeID := r.PathValue("nodeID")
	now := time.Now().UTC()
	var response map[string]any
	err := s.store.Update(func(state *core.State) error {
		node := state.Nodes[nodeID]
		if node == nil {
			return fmt.Errorf("node not found")
		}
		node.Status = target
		node.UpdatedAt = now
		if target == core.NodeDisabled {
			node.DisabledAt = &now
			node.TokenHash = core.HashSecret(newIDOrFallback("revoked"))
		} else {
			node.DisabledAt = nil
		}
		if expireLeases {
			for _, lease := range state.Leases {
				if lease.NodeID == nodeID && lease.Status == core.LeaseActive {
					s.expireLeaseLocked(state, lease, now, "node disabled by administrator")
				}
			}
			for _, lease := range state.ResourceLeases {
				if lease.NodeID == nodeID && lease.Status == core.ResourceLeaseActive {
					lease.Status = core.ResourceLeaseExpired
					lease.UpdatedAt = now
					lease.ReleasedAt = &now
					lease.RegistryRevision = bumpRegistry(state, s.cfg.EventRetention, "resource_lease.expired", "resource_lease", lease.ID, map[string]string{"reason": "node disabled"})
					if sandbox := state.Sandboxes[lease.SandboxID]; sandbox != nil && sandbox.Status != core.SandboxDestroyed {
						sandbox.Status = core.SandboxStopped
						sandbox.Error = "node disabled"
						sandbox.UpdatedAt = now
					}
				}
			}
		}
		response = publicNode(node)
		addEvent(state, s.cfg.EventRetention, "node.status_changed", actorFromRequest(r), "node", nodeID, "node status changed", map[string]string{"status": target})
		bumpRegistry(state, s.cfg.EventRetention, "device.status_changed", "device", nodeID, map[string]string{"status": target})
		return nil
	})
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "node_not_found", err.Error())
		return
	}
	s.notifyRegistryChanged()
	writeJSON(w, http.StatusOK, map[string]any{"node": response})
}

func (s *Server) handleNodeDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	nodeID := r.PathValue("nodeID")
	err := s.store.Update(func(state *core.State) error {
		node := state.Nodes[nodeID]
		if node == nil {
			return fmt.Errorf("node not found")
		}
		for _, lease := range state.Leases {
			if lease.NodeID == nodeID && lease.Status == core.LeaseActive {
				return fmt.Errorf("node has active leases; drain or disable it first")
			}
		}
		for _, lease := range state.ResourceLeases {
			if lease.NodeID == nodeID && lease.Status == core.ResourceLeaseActive {
				return fmt.Errorf("node has active resource leases; release them first")
			}
		}
		for _, sandbox := range state.Sandboxes {
			if sandbox.NodeID == nodeID && sandbox.Status != core.SandboxDestroyed {
				return fmt.Errorf("node has persistent sandboxes; destroy them first")
			}
		}
		delete(state.Nodes, nodeID)
		addEvent(state, s.cfg.EventRetention, "node.deleted", actorFromRequest(r), "node", nodeID, "node registration deleted", map[string]string{"name": node.Name})
		bumpRegistry(state, s.cfg.EventRetention, "device.deleted", "device", nodeID, map[string]string{"name": node.Name})
		return nil
	})
	if err != nil {
		writeAPIError(w, http.StatusConflict, "node_delete_failed", err.Error())
		return
	}
	s.notifyRegistryChanged()
	w.WriteHeader(http.StatusNoContent)
}

type taskSubmitResponse struct {
	Task    *core.Task `json:"task"`
	Created bool       `json:"created"`
}

func (s *Server) handleTaskSubmit(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var spec core.TaskSpec
	if !decodeJSON(w, r, s.cfg.MaxRequestBytes, &spec) {
		return
	}
	if err := normalizeTaskSpec(&spec); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_task", err.Error())
		return
	}
	now := time.Now().UTC()
	response := taskSubmitResponse{}
	err := s.store.Update(func(state *core.State) error {
		for _, input := range spec.Inputs {
			if state.Artifacts[input.ArtifactID] == nil {
				return fmt.Errorf("input artifact %s does not exist", input.ArtifactID)
			}
		}
		if spec.IdempotencyKey != "" {
			if existingID := state.Idempotency[spec.IdempotencyKey]; existingID != "" {
				if existing := state.Tasks[existingID]; existing != nil {
					copy := cloneViaJSON(*existing)
					response.Task = &copy
					response.Created = false
					return nil
				}
			}
		}
		taskID, idErr := core.NewID("task")
		if idErr != nil {
			return idErr
		}
		task := &core.Task{ID: taskID, Spec: spec, Status: core.TaskQueued, CreatedAt: now, UpdatedAt: now}
		state.Tasks[taskID] = task
		if spec.IdempotencyKey != "" {
			state.Idempotency[spec.IdempotencyKey] = taskID
		}
		copy := cloneViaJSON(*task)
		response.Task = &copy
		response.Created = true
		addEvent(state, s.cfg.EventRetention, "task.created", actorFromRequest(r), "task", taskID, "task submitted", map[string]string{"name": spec.Name, "queue": spec.Queue})
		return nil
	})
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "task_submit_failed", err.Error())
		return
	}
	status := http.StatusOK
	if response.Created {
		status = http.StatusCreated
	}
	writeJSON(w, status, response)
}

func (s *Server) handleTasksList(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	limit := parseLimit(r, 100, 1000)
	statusFilter := strings.TrimSpace(r.URL.Query().Get("status"))
	queueFilter := strings.TrimSpace(r.URL.Query().Get("queue"))
	nodeFilter := strings.TrimSpace(r.URL.Query().Get("node_id"))
	tasks := []*core.Task{}
	_ = s.store.View(func(state *core.State) error {
		for _, task := range state.Tasks {
			if statusFilter != "" && task.Status != statusFilter {
				continue
			}
			if queueFilter != "" && task.Spec.Queue != queueFilter {
				continue
			}
			if nodeFilter != "" && task.AssignedNodeID != nodeFilter {
				continue
			}
			copy := cloneViaJSON(*task)
			tasks = append(tasks, &copy)
		}
		return nil
	})
	sort.Slice(tasks, func(i, j int) bool {
		if !tasks[i].CreatedAt.Equal(tasks[j].CreatedAt) {
			return tasks[i].CreatedAt.After(tasks[j].CreatedAt)
		}
		return tasks[i].ID < tasks[j].ID
	})
	if len(tasks) > limit {
		tasks = tasks[:limit]
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": tasks, "count": len(tasks)})
}

func (s *Server) handleTaskGet(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	taskID := r.PathValue("taskID")
	var task *core.Task
	executions := []*core.Execution{}
	var lease map[string]any
	_ = s.store.View(func(state *core.State) error {
		if current := state.Tasks[taskID]; current != nil {
			copy := cloneViaJSON(*current)
			task = &copy
			for _, execution := range state.Executions {
				if execution.TaskID == taskID {
					execCopy := cloneViaJSON(*execution)
					executions = append(executions, &execCopy)
				}
			}
			if current.ActiveLeaseID != "" {
				if currentLease := state.Leases[current.ActiveLeaseID]; currentLease != nil {
					lease = map[string]any{"id": currentLease.ID, "task_id": currentLease.TaskID, "node_id": currentLease.NodeID, "execution_id": currentLease.ExecutionID, "status": currentLease.Status, "created_at": currentLease.CreatedAt, "last_renewed_at": currentLease.LastRenewedAt, "expires_at": currentLease.ExpiresAt}
				}
			}
		}
		return nil
	})
	if task == nil {
		writeAPIError(w, http.StatusNotFound, "task_not_found", "task not found")
		return
	}
	sort.Slice(executions, func(i, j int) bool { return executions[i].Attempt < executions[j].Attempt })
	writeJSON(w, http.StatusOK, map[string]any{"task": task, "executions": executions, "active_lease": lease})
}

func (s *Server) handleTaskDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	taskID := r.PathValue("taskID")
	found := false
	executionIDs := []string{}
	err := s.store.Update(func(state *core.State) error {
		task := state.Tasks[taskID]
		if task == nil {
			return fmt.Errorf("task not found")
		}
		found = true
		if !core.IsTerminalTaskStatus(task.Status) || task.ActiveLeaseID != "" {
			return fmt.Errorf("task must be terminal and have no active lease before deletion")
		}
		for leaseID, lease := range state.Leases {
			if lease.TaskID != taskID {
				continue
			}
			if lease.Status == core.LeaseActive {
				return fmt.Errorf("task still has active lease %s", leaseID)
			}
			delete(state.Leases, leaseID)
		}
		for executionID, execution := range state.Executions {
			if execution.TaskID == taskID {
				executionIDs = append(executionIDs, executionID)
				delete(state.Executions, executionID)
			}
		}
		for key, mappedTaskID := range state.Idempotency {
			if mappedTaskID == taskID {
				delete(state.Idempotency, key)
			}
		}
		deletedExecutions := map[string]bool{}
		for _, executionID := range executionIDs {
			deletedExecutions[executionID] = true
		}
		for _, artifact := range state.Artifacts {
			if artifact.ProducerTaskID == taskID {
				artifact.ProducerTaskID = ""
			}
			if deletedExecutions[artifact.ProducerExecutionID] {
				artifact.ProducerExecutionID = ""
			}
		}
		delete(state.Tasks, taskID)
		state.Events = filterEvents(state.Events, "task", taskID)
		addEvent(state, s.cfg.EventRetention, "task.deleted", actorFromRequest(r), "task", taskID, "terminal task and execution history deleted", map[string]string{"executions": strconv.Itoa(len(executionIDs))})
		return nil
	})
	if err != nil {
		status, code := http.StatusConflict, "task_delete_failed"
		if !found {
			status, code = http.StatusNotFound, "task_not_found"
		}
		writeAPIError(w, status, code, err.Error())
		return
	}
	sort.Strings(executionIDs)
	warnings := []string{}
	deletedLogs := 0
	for _, executionID := range executionIDs {
		if err := s.logs.Delete(executionID); err != nil {
			warnings = append(warnings, fmt.Sprintf("could not delete log for %s", executionID))
			s.logger.Warn("task deletion left an execution log", "task_id", taskID, "execution_id", executionID, "error", err)
			continue
		}
		deletedLogs++
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "task_id": taskID, "executions_deleted": len(executionIDs), "logs_deleted": deletedLogs, "warnings": warnings})
}

func filterEvents(events []core.Event, resourceType, resourceID string) []core.Event {
	filtered := make([]core.Event, 0, len(events))
	for _, event := range events {
		if event.ResourceType == resourceType && event.ResourceID == resourceID {
			continue
		}
		filtered = append(filtered, event)
	}
	return filtered
}

func (s *Server) handleTaskCancel(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	taskID := r.PathValue("taskID")
	now := time.Now().UTC()
	var taskCopy *core.Task
	err := s.store.Update(func(state *core.State) error {
		task := state.Tasks[taskID]
		if task == nil {
			return fmt.Errorf("task not found")
		}
		if task.Status == core.TaskCancelled {
			copy := cloneViaJSON(*task)
			taskCopy = &copy
			return nil
		}
		if core.IsTerminalTaskStatus(task.Status) {
			return fmt.Errorf("task is already terminal with status %s", task.Status)
		}
		task.CancelRequested = true
		task.UpdatedAt = now
		if task.Status == core.TaskQueued || task.Status == core.TaskRetryWait {
			task.Status = core.TaskCancelled
			task.FinishedAt = &now
			task.NextEligibleAt = nil
		}
		copy := cloneViaJSON(*task)
		taskCopy = &copy
		addEvent(state, s.cfg.EventRetention, "task.cancel_requested", actorFromRequest(r), "task", taskID, "task cancellation requested", map[string]string{"status": task.Status})
		return nil
	})
	if err != nil {
		writeAPIError(w, http.StatusConflict, "task_cancel_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"task": taskCopy})
}

type taskRetryRequest struct {
	ResetAttempts            bool `json:"reset_attempts,omitempty"`
	AcknowledgeNonIdempotent bool `json:"acknowledge_non_idempotent,omitempty"`
}

func (s *Server) handleTaskRetry(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var request taskRetryRequest
	if r.ContentLength != 0 {
		if !decodeJSON(w, r, s.cfg.MaxRequestBytes, &request) {
			return
		}
	}
	taskID := r.PathValue("taskID")
	now := time.Now().UTC()
	var taskCopy *core.Task
	err := s.store.Update(func(state *core.State) error {
		task := state.Tasks[taskID]
		if task == nil {
			return fmt.Errorf("task not found")
		}
		if task.ActiveLeaseID != "" || task.Status == core.TaskRunning || task.Status == core.TaskLeased {
			return fmt.Errorf("task still has an active execution")
		}
		if !core.IsTerminalTaskStatus(task.Status) && task.Status != core.TaskRetryWait {
			return fmt.Errorf("task status %s cannot be manually retried", task.Status)
		}
		if task.Spec.SideEffectMode == core.SideEffectNonIdempotent && !request.AcknowledgeNonIdempotent {
			return fmt.Errorf("retrying a non-idempotent task requires acknowledge_non_idempotent=true")
		}
		if request.ResetAttempts {
			task.Attempts = 0
		}
		if task.Attempts >= task.Spec.MaxAttempts && !request.ResetAttempts {
			return fmt.Errorf("retry budget exhausted; use reset_attempts=true to create a fresh budget")
		}
		task.Status = core.TaskQueued
		task.CancelRequested = false
		task.AssignedNodeID = ""
		task.ActiveLeaseID = ""
		task.UpdatedAt = now
		task.FinishedAt = nil
		task.NextEligibleAt = nil
		task.LastError = ""
		task.Result = nil
		copy := cloneViaJSON(*task)
		taskCopy = &copy
		addEvent(state, s.cfg.EventRetention, "task.manual_retry", actorFromRequest(r), "task", taskID, "task manually returned to queue", map[string]string{"reset_attempts": strconv.FormatBool(request.ResetAttempts)})
		return nil
	})
	if err != nil {
		writeAPIError(w, http.StatusConflict, "task_retry_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"task": taskCopy})
}

func (s *Server) handleEventsList(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	limit := parseLimit(r, 100, 1000)
	eventType := strings.TrimSpace(r.URL.Query().Get("type"))
	events := []core.Event{}
	_ = s.store.View(func(state *core.State) error {
		for index := len(state.Events) - 1; index >= 0 && len(events) < limit; index-- {
			event := state.Events[index]
			if eventType != "" && event.Type != eventType {
				continue
			}
			events = append(events, cloneViaJSON(event))
		}
		return nil
	})
	writeJSON(w, http.StatusOK, map[string]any{"events": events, "count": len(events)})
}
