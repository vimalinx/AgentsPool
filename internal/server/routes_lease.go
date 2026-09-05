package server

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/qiyuehuajin/agentspool/internal/core"
	"github.com/qiyuehuajin/agentspool/internal/store"
)

type leaseRenewResponse struct {
	ExpiresAt       time.Time `json:"expires_at"`
	CancelRequested bool      `json:"cancel_requested"`
	ServerTime      time.Time `json:"server_time"`
}

type leaseLogRequest struct {
	Events []store.LogEvent `json:"events"`
}

type leaseCompleteRequest struct {
	ExitCode          int               `json:"exit_code"`
	Summary           string            `json:"summary,omitempty"`
	OutputArtifactIDs []string          `json:"output_artifact_ids,omitempty"`
	Metadata          map[string]string `json:"metadata,omitempty"`
}

type leaseFailRequest struct {
	ExitCode          int      `json:"exit_code"`
	Error             string   `json:"error"`
	OutputArtifactIDs []string `json:"output_artifact_ids,omitempty"`
}

func (s *Server) handleLeaseStart(w http.ResponseWriter, r *http.Request) {
	leaseID := r.PathValue("leaseID")
	if _, ok := s.authenticateLease(w, r, leaseID); !ok {
		return
	}
	now := time.Now().UTC()
	var taskCopy core.Task
	err := s.store.Update(func(state *core.State) error {
		lease, task, execution, err := ensureActiveLease(state, leaseID, now)
		if err != nil {
			return err
		}
		if execution.Status == core.ExecutionRunning {
			taskCopy = cloneViaJSON(*task)
			return nil
		}
		if execution.Status != core.ExecutionLeased {
			return fmt.Errorf("execution cannot be started from status %s", execution.Status)
		}
		execution.Status = core.ExecutionRunning
		execution.StartedAt = &now
		task.Status = core.TaskRunning
		task.UpdatedAt = now
		if task.StartedAt == nil {
			task.StartedAt = &now
		}
		lease.LastRenewedAt = now
		lease.ExpiresAt = now.Add(s.cfg.LeaseTTL)
		taskCopy = cloneViaJSON(*task)
		addEvent(state, s.cfg.EventRetention, "execution.started", "node:"+lease.NodeID, "execution", execution.ID, "worker started task execution", map[string]string{"task_id": task.ID, "lease_id": lease.ID})
		return nil
	})
	if err != nil {
		writeAPIError(w, http.StatusConflict, "lease_not_active", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"task": taskCopy, "server_time": now})
}

func (s *Server) handleLeaseRenew(w http.ResponseWriter, r *http.Request) {
	leaseID := r.PathValue("leaseID")
	if _, ok := s.authenticateLease(w, r, leaseID); !ok {
		return
	}
	now := time.Now().UTC()
	var response leaseRenewResponse
	err := s.store.UpdateVolatile(func(state *core.State) error {
		lease, task, _, err := ensureActiveLease(state, leaseID, now)
		if err != nil {
			return err
		}
		lease.LastRenewedAt = now
		lease.ExpiresAt = now.Add(s.cfg.LeaseTTL)
		if node := state.Nodes[lease.NodeID]; node != nil {
			node.LastSeenAt = now
			node.UpdatedAt = now
			if node.Status == core.NodeOffline {
				node.Status = core.NodeOnline
			}
		}
		response = leaseRenewResponse{ExpiresAt: lease.ExpiresAt, CancelRequested: task.CancelRequested, ServerTime: now}
		return nil
	})
	if err != nil {
		writeAPIError(w, http.StatusConflict, "lease_not_active", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleLeaseLogs(w http.ResponseWriter, r *http.Request) {
	leaseID := r.PathValue("leaseID")
	lease, ok := s.authenticateLease(w, r, leaseID)
	if !ok {
		return
	}
	var request leaseLogRequest
	if !decodeJSON(w, r, s.cfg.MaxRequestBytes, &request) {
		return
	}
	if len(request.Events) > 1000 {
		writeAPIError(w, http.StatusBadRequest, "too_many_log_events", "a log batch may contain at most 1000 events")
		return
	}
	active := false
	_ = s.store.View(func(state *core.State) error {
		current := state.Leases[leaseID]
		active = current != nil && current.Status == core.LeaseActive && current.ExpiresAt.After(time.Now().UTC())
		return nil
	})
	if !active {
		writeAPIError(w, http.StatusConflict, "lease_not_active", "lease is no longer active")
		return
	}
	now := time.Now().UTC()
	for i := range request.Events {
		if request.Events[i].Timestamp.IsZero() {
			request.Events[i].Timestamp = now
		}
		if request.Events[i].Stream != "stdout" && request.Events[i].Stream != "stderr" && request.Events[i].Stream != "system" {
			writeAPIError(w, http.StatusBadRequest, "invalid_log_stream", "log stream must be stdout, stderr, or system")
			return
		}
		if len(request.Events[i].Data) > 256<<10 {
			writeAPIError(w, http.StatusBadRequest, "log_event_too_large", "a log event may not exceed 256 KiB")
			return
		}
	}
	if err := s.logs.Append(lease.ExecutionID, request.Events); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "log_write_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"accepted": len(request.Events)})
}

func (s *Server) handleLeaseComplete(w http.ResponseWriter, r *http.Request) {
	leaseID := r.PathValue("leaseID")
	if _, ok := s.authenticateLease(w, r, leaseID); !ok {
		return
	}
	var request leaseCompleteRequest
	if !decodeJSON(w, r, s.cfg.MaxRequestBytes, &request) {
		return
	}
	if request.ExitCode != 0 {
		writeAPIError(w, http.StatusBadRequest, "nonzero_exit_code", "successful completion requires exit_code 0")
		return
	}
	now := time.Now().UTC()
	var taskCopy core.Task
	err := s.store.Update(func(state *core.State) error {
		lease, task, execution, err := ensureActiveLease(state, leaseID, now)
		if err != nil {
			return err
		}
		if err := validateOutputArtifacts(state, execution.ID, request.OutputArtifactIDs); err != nil {
			return err
		}
		lease.Status = core.LeaseCompleted
		lease.FinishedAt = &now
		syncNodeActiveJobsLocked(state, lease.NodeID, now)
		execution.Status = core.ExecutionSucceeded
		execution.FinishedAt = &now
		execution.ExitCode = &request.ExitCode
		execution.OutputArtifactIDs = append([]string(nil), request.OutputArtifactIDs...)
		task.AssignedNodeID = ""
		task.ActiveLeaseID = ""
		task.UpdatedAt = now
		task.FinishedAt = &now
		task.Result = &core.TaskResult{ExitCode: request.ExitCode, Summary: request.Summary, OutputArtifactIDs: append([]string(nil), request.OutputArtifactIDs...), Metadata: request.Metadata}
		if task.CancelRequested {
			task.Status = core.TaskCancelled
		} else {
			task.Status = core.TaskSucceeded
		}
		taskCopy = cloneViaJSON(*task)
		addEvent(state, s.cfg.EventRetention, "task.completed", "node:"+lease.NodeID, "task", task.ID, "task execution completed successfully", map[string]string{"execution_id": execution.ID})
		return nil
	})
	if err != nil {
		writeAPIError(w, http.StatusConflict, "completion_rejected", err.Error())
		return
	}
	s.metrics.CompletionsTotal.Add(1)
	writeJSON(w, http.StatusOK, map[string]any{"task": taskCopy})
}

func (s *Server) handleLeaseFail(w http.ResponseWriter, r *http.Request) {
	leaseID := r.PathValue("leaseID")
	if _, ok := s.authenticateLease(w, r, leaseID); !ok {
		return
	}
	var request leaseFailRequest
	if !decodeJSON(w, r, s.cfg.MaxRequestBytes, &request) {
		return
	}
	request.Error = strings.TrimSpace(request.Error)
	if request.Error == "" {
		request.Error = "worker reported execution failure"
	}
	if len(request.Error) > 32<<10 {
		request.Error = request.Error[:32<<10]
	}
	now := time.Now().UTC()
	var taskCopy core.Task
	err := s.store.Update(func(state *core.State) error {
		lease, task, execution, err := ensureActiveLease(state, leaseID, now)
		if err != nil {
			return err
		}
		if err := validateOutputArtifacts(state, execution.ID, request.OutputArtifactIDs); err != nil {
			return err
		}
		applyFailureLocked(state, task, lease, execution, now, request.ExitCode, request.Error, request.OutputArtifactIDs)
		syncNodeActiveJobsLocked(state, lease.NodeID, now)
		taskCopy = cloneViaJSON(*task)
		addEvent(state, s.cfg.EventRetention, "task.failed", "node:"+lease.NodeID, "task", task.ID, "task execution failed", map[string]string{"execution_id": execution.ID, "error": request.Error, "status": task.Status})
		return nil
	})
	if err != nil {
		writeAPIError(w, http.StatusConflict, "failure_rejected", err.Error())
		return
	}
	s.metrics.FailuresTotal.Add(1)
	writeJSON(w, http.StatusOK, map[string]any{"task": taskCopy})
}

func (s *Server) handleLeaseCancelled(w http.ResponseWriter, r *http.Request) {
	leaseID := r.PathValue("leaseID")
	if _, ok := s.authenticateLease(w, r, leaseID); !ok {
		return
	}
	now := time.Now().UTC()
	var taskCopy core.Task
	err := s.store.Update(func(state *core.State) error {
		lease, task, execution, err := ensureActiveLease(state, leaseID, now)
		if err != nil {
			return err
		}
		lease.Status = core.LeaseCancelled
		lease.FinishedAt = &now
		syncNodeActiveJobsLocked(state, lease.NodeID, now)
		execution.Status = core.ExecutionCancelled
		execution.FinishedAt = &now
		task.Status = core.TaskCancelled
		task.CancelRequested = true
		task.AssignedNodeID = ""
		task.ActiveLeaseID = ""
		task.UpdatedAt = now
		task.FinishedAt = &now
		taskCopy = cloneViaJSON(*task)
		addEvent(state, s.cfg.EventRetention, "task.cancelled", "node:"+lease.NodeID, "task", task.ID, "worker acknowledged cancellation", map[string]string{"execution_id": execution.ID})
		return nil
	})
	if err != nil {
		writeAPIError(w, http.StatusConflict, "cancellation_rejected", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"task": taskCopy})
}

func validateOutputArtifacts(state *core.State, executionID string, artifactIDs []string) error {
	if len(artifactIDs) > 10000 {
		return fmt.Errorf("too many output artifacts")
	}
	for _, artifactID := range artifactIDs {
		artifact := state.Artifacts[artifactID]
		if artifact == nil {
			return fmt.Errorf("output artifact %s does not exist", artifactID)
		}
		if artifact.ProducerExecutionID != executionID {
			return fmt.Errorf("artifact %s was not produced by this execution", artifactID)
		}
	}
	return nil
}
