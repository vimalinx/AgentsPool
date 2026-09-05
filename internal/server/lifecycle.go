package server

import (
	"context"
	"fmt"
	"time"

	"github.com/qiyuehuajin/agentspool/internal/core"
)

func (s *Server) recoverState() error {
	now := time.Now().UTC()
	return s.store.Update(func(state *core.State) error {
		changed := false
		for _, node := range state.Nodes {
			if node.Status != core.NodeDisabled {
				node.Status = core.NodeOffline
				node.UpdatedAt = now
				changed = true
			}
		}
		for _, lease := range state.Leases {
			if lease.Status != core.LeaseActive {
				continue
			}
			s.expireLeaseLocked(state, lease, now, "control plane restarted")
			changed = true
		}
		for _, operation := range state.SandboxOperations {
			if operation.Status != core.SandboxOperationClaimed && operation.Status != core.SandboxOperationRunning {
				continue
			}
			if operation.Kind == "exec" {
				// The Worker and its systemd unit are independent of the control-plane
				// process. Keep the authenticated claim intact so a live Worker may
				// continue reporting, and so an operator may still cancel the unit.
				operation.Status = core.SandboxOperationOrphaned
				operation.Error = "control plane restarted; waiting for worker reconciliation"
			} else {
				operation.Status = core.SandboxOperationQueued
				operation.ClaimedAt = nil
				operation.ClaimTokenHash = ""
			}
			operation.UpdatedAt = now
			changed = true
		}
		if changed {
			addEvent(state, s.cfg.EventRetention, "server.recovered", "system", "server", "control-plane", "recovered persisted state after startup", nil)
			bumpRegistry(state, s.cfg.EventRetention, "registry.recovered", "registry", "control-plane", nil)
		}
		return nil
	})
}

func (s *Server) runReaper(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.ReaperInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if err := s.reap(now.UTC()); err != nil {
				s.logger.Error("lease/node reaper failed", "error", err)
			}
		}
	}
}

func (s *Server) reap(now time.Time) error {
	needsUpdate := false
	_ = s.store.View(func(state *core.State) error {
		for _, node := range state.Nodes {
			if node.Status == core.NodeOnline && now.Sub(node.LastSeenAt) > s.cfg.NodeTimeout {
				needsUpdate = true
				return nil
			}
		}
		for _, lease := range state.Leases {
			if lease.Status == core.LeaseActive && !lease.ExpiresAt.After(now) {
				needsUpdate = true
				return nil
			}
		}
		for _, lease := range state.ResourceLeases {
			if lease.Status == core.ResourceLeaseActive && lease.ExpiresAt != nil && !lease.ExpiresAt.After(now) {
				needsUpdate = true
				return nil
			}
		}
		for _, task := range state.Tasks {
			if task.Status == core.TaskRetryWait && task.NextEligibleAt != nil && !task.NextEligibleAt.After(now) {
				needsUpdate = true
				return nil
			}
		}
		return nil
	})
	if !needsUpdate {
		return nil
	}

	changedRegistry := false
	err := s.store.Update(func(state *core.State) error {
		for _, node := range state.Nodes {
			if node.Status == core.NodeOnline && now.Sub(node.LastSeenAt) > s.cfg.NodeTimeout {
				node.Status = core.NodeOffline
				node.UpdatedAt = now
				addEvent(state, s.cfg.EventRetention, "node.offline", "system", "node", node.ID, "node heartbeat timed out", nil)
				bumpRegistry(state, s.cfg.EventRetention, "device.offline", "device", node.ID, nil)
				changedRegistry = true
				for _, operation := range state.SandboxOperations {
					if operation.NodeID != node.ID || (operation.Status != core.SandboxOperationClaimed && operation.Status != core.SandboxOperationRunning) {
						continue
					}
					if operation.Kind == "exec" {
						operation.Status = core.SandboxOperationOrphaned
						operation.Error = "worker went offline; execution state awaits reconciliation"
					} else {
						operation.Status = core.SandboxOperationQueued
						operation.ClaimedAt = nil
						operation.ClaimTokenHash = ""
					}
					operation.UpdatedAt = now
				}
			}
		}
		for _, lease := range state.ResourceLeases {
			if lease.Status == core.ResourceLeaseActive && lease.ExpiresAt != nil && !lease.ExpiresAt.After(now) {
				lease.Status = core.ResourceLeaseExpired
				lease.UpdatedAt = now
				lease.ReleasedAt = &now
				lease.RegistryRevision = bumpRegistry(state, s.cfg.EventRetention, "resource_lease.expired", "resource_lease", lease.ID, nil)
				if lease.SandboxID != "" {
					if sandbox := state.Sandboxes[lease.SandboxID]; sandbox != nil && sandbox.Status != core.SandboxDestroyed {
						sandbox.Status = core.SandboxStopped
						sandbox.UpdatedAt = now
						sandbox.Error = "resource lease expired"
					}
				}
				changedRegistry = true
			}
		}
		for _, lease := range state.Leases {
			if lease.Status == core.LeaseActive && !lease.ExpiresAt.After(now) {
				s.expireLeaseLocked(state, lease, now, "lease expired")
				s.metrics.LeaseExpirations.Add(1)
			}
		}
		for _, task := range state.Tasks {
			if task.Status == core.TaskRetryWait && task.NextEligibleAt != nil && !task.NextEligibleAt.After(now) {
				task.Status = core.TaskQueued
				task.NextEligibleAt = nil
				task.UpdatedAt = now
				addEvent(state, s.cfg.EventRetention, "task.requeued", "system", "task", task.ID, "retry backoff elapsed", nil)
			}
		}
		return nil
	})
	if err == nil && changedRegistry {
		s.notifyRegistryChanged()
	}
	return err
}

func activeLeaseCountForNode(state *core.State, nodeID string, now time.Time) int {
	count := 0
	for _, lease := range state.Leases {
		if lease.NodeID == nodeID && lease.Status == core.LeaseActive && lease.ExpiresAt.After(now) {
			count++
		}
	}
	return count
}

func syncNodeActiveJobsLocked(state *core.State, nodeID string, now time.Time) int {
	count := activeLeaseCountForNode(state, nodeID, now)
	if node := state.Nodes[nodeID]; node != nil {
		node.Resources.ActiveJobs = count
	}
	return count
}

func (s *Server) expireLeaseLocked(state *core.State, lease *core.Lease, now time.Time, reason string) {
	if lease.Status != core.LeaseActive {
		return
	}
	lease.Status = core.LeaseExpired
	lease.FinishedAt = &now
	syncNodeActiveJobsLocked(state, lease.NodeID, now)

	task := state.Tasks[lease.TaskID]
	execution := state.Executions[lease.ExecutionID]
	started := false
	if execution != nil {
		started = execution.Status == core.ExecutionRunning
		execution.Status = core.ExecutionOrphaned
		execution.Error = reason
		execution.FinishedAt = &now
	}
	if task == nil {
		return
	}
	task.AssignedNodeID = ""
	task.ActiveLeaseID = ""
	task.UpdatedAt = now
	task.LastError = reason

	if task.CancelRequested {
		task.Status = core.TaskCancelled
		task.FinishedAt = &now
		addEvent(state, s.cfg.EventRetention, "task.cancelled", "system", "task", task.ID, "cancelled after worker lease ended", nil)
		return
	}
	if started && task.Spec.SideEffectMode == core.SideEffectNonIdempotent {
		task.Status = core.TaskNeedsReview
		task.FinishedAt = &now
		addEvent(state, s.cfg.EventRetention, "task.needs_review", "system", "task", task.ID, "non-idempotent execution lost its lease; automatic replay blocked", map[string]string{"reason": reason})
		return
	}
	if task.Attempts < task.Spec.MaxAttempts {
		delay := retryDelay(task)
		next := now.Add(delay)
		task.Status = core.TaskRetryWait
		task.NextEligibleAt = &next
		addEvent(state, s.cfg.EventRetention, "task.retry_scheduled", "system", "task", task.ID, "lease loss scheduled for retry", map[string]string{"reason": reason, "retry_at": next.Format(time.RFC3339Nano)})
		return
	}
	task.Status = core.TaskDeadLetter
	task.FinishedAt = &now
	addEvent(state, s.cfg.EventRetention, "task.dead_letter", "system", "task", task.ID, "lease lost and retry budget exhausted", map[string]string{"reason": reason})
}

func retryDelay(task *core.Task) time.Duration {
	base := task.Spec.RetryBackoffSeconds
	if base <= 0 {
		base = 5
	}
	exponent := task.Attempts - 1
	if exponent < 0 {
		exponent = 0
	}
	const maxDuration = time.Duration(1<<63 - 1)
	maxSeconds := int64(maxDuration / time.Second)
	seconds := int64(base)
	if seconds >= maxSeconds {
		return time.Duration(maxSeconds) * time.Second
	}
	for step := 0; step < exponent; step++ {
		if seconds > maxSeconds/2 {
			seconds = maxSeconds
			break
		}
		seconds *= 2
	}
	return time.Duration(seconds) * time.Second
}

func applyFailureLocked(state *core.State, task *core.Task, lease *core.Lease, execution *core.Execution, now time.Time, exitCode int, reason string, outputArtifacts []string) {
	started := execution.Status == core.ExecutionRunning
	lease.Status = core.LeaseFailed
	lease.FinishedAt = &now
	execution.Status = core.ExecutionFailed
	execution.FinishedAt = &now
	execution.ExitCode = &exitCode
	execution.Error = reason
	execution.OutputArtifactIDs = append([]string(nil), outputArtifacts...)
	task.AssignedNodeID = ""
	task.ActiveLeaseID = ""
	task.UpdatedAt = now
	task.LastError = reason
	task.Result = &core.TaskResult{ExitCode: exitCode, OutputArtifactIDs: append([]string(nil), outputArtifacts...)}

	canRetry := task.Spec.RetryOnFailure && task.Attempts < task.Spec.MaxAttempts && (!started || task.Spec.SideEffectMode != core.SideEffectNonIdempotent) && !task.CancelRequested
	if canRetry {
		next := now.Add(retryDelay(task))
		task.Status = core.TaskRetryWait
		task.NextEligibleAt = &next
		task.FinishedAt = nil
		return
	}
	if task.CancelRequested {
		task.Status = core.TaskCancelled
	} else if task.Spec.RetryOnFailure && task.Attempts >= task.Spec.MaxAttempts {
		task.Status = core.TaskDeadLetter
	} else {
		task.Status = core.TaskFailed
	}
	task.FinishedAt = &now
}

func ensureActiveLease(state *core.State, leaseID string, now time.Time) (*core.Lease, *core.Task, *core.Execution, error) {
	lease := state.Leases[leaseID]
	if lease == nil {
		return nil, nil, nil, fmt.Errorf("lease not found")
	}
	if lease.Status != core.LeaseActive {
		return nil, nil, nil, fmt.Errorf("lease is not active")
	}
	if !lease.ExpiresAt.After(now) {
		return nil, nil, nil, fmt.Errorf("lease has expired")
	}
	task := state.Tasks[lease.TaskID]
	execution := state.Executions[lease.ExecutionID]
	if task == nil || execution == nil {
		return nil, nil, nil, fmt.Errorf("lease references missing task or execution")
	}
	return lease, task, execution, nil
}
