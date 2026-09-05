package server

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/qiyuehuajin/agentspool/internal/config"
	"github.com/qiyuehuajin/agentspool/internal/core"
	"github.com/qiyuehuajin/agentspool/internal/store"
)

func leaseState(sideEffect, executionStatus string, attempts, maxAttempts int) (core.State, *core.Lease, *core.Task) {
	now := time.Now().UTC()
	state := core.NewState()
	task := &core.Task{
		ID: "task_test", Status: core.TaskRunning, Attempts: attempts,
		ActiveLeaseID: "lease_test", AssignedNodeID: "node_test",
		Spec: core.TaskSpec{SideEffectMode: sideEffect, MaxAttempts: maxAttempts, RetryBackoffSeconds: 1},
	}
	lease := &core.Lease{ID: "lease_test", TaskID: task.ID, ExecutionID: "exec_test", NodeID: "node_test", Status: core.LeaseActive, ExpiresAt: now.Add(time.Minute)}
	execution := &core.Execution{ID: "exec_test", TaskID: task.ID, LeaseID: lease.ID, NodeID: lease.NodeID, Status: executionStatus}
	state.Tasks[task.ID] = task
	state.Leases[lease.ID] = lease
	state.Executions[execution.ID] = execution
	return state, lease, task
}

func TestControlPlaneRecoveryPreservesRunningExecClaim(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "state")
	stateStore, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := stateStore.Update(func(state *core.State) error {
		state.Nodes["node_test"] = &core.Node{ID: "node_test", Status: core.NodeOnline}
		state.SandboxOperations["sbxop_test"] = &core.SandboxOperation{
			ID: "sbxop_test", SandboxID: "sbx_test", NodeID: "node_test", Kind: "exec",
			Status: core.SandboxOperationRunning, ClaimTokenHash: core.HashSecret("operation-token"),
			CreatedAt: now, UpdatedAt: now, StartedAt: &now,
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultServer()
	cfg.DataDir = dataDir
	cfg.AdminToken = testAdminToken
	cfg.AllocatorToken = testAllocatorToken
	cfg.JoinToken = testJoinToken
	if _, err := New(cfg, stateStore, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	if err := stateStore.View(func(state *core.State) error {
		operation := state.SandboxOperations["sbxop_test"]
		if operation.Status != core.SandboxOperationOrphaned || operation.ClaimTokenHash == "" || !core.SecureHashEqual(operation.ClaimTokenHash, "operation-token") {
			t.Fatalf("running exec claim was not recoverable: %#v", operation)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestLostStartedNonIdempotentTaskNeedsReview(t *testing.T) {
	state, lease, task := leaseState(core.SideEffectNonIdempotent, core.ExecutionRunning, 1, 3)
	s := &Server{cfg: config.Server{EventRetention: 100}}
	s.expireLeaseLocked(&state, lease, time.Now().UTC(), "test disconnect")
	if task.Status != core.TaskNeedsReview {
		t.Fatalf("expected needs_review, got %s", task.Status)
	}
	if task.ActiveLeaseID != "" || task.AssignedNodeID != "" {
		t.Fatal("expired task retained lease ownership")
	}
}

func TestUnstartedNonIdempotentTaskCanRetry(t *testing.T) {
	state, lease, task := leaseState(core.SideEffectNonIdempotent, core.ExecutionLeased, 1, 3)
	s := &Server{cfg: config.Server{EventRetention: 100}}
	s.expireLeaseLocked(&state, lease, time.Now().UTC(), "test disconnect")
	if task.Status != core.TaskRetryWait {
		t.Fatalf("expected retry_wait before execution began, got %s", task.Status)
	}
	if task.NextEligibleAt == nil {
		t.Fatal("retry task has no next eligible time")
	}
}

func TestLostTaskExhaustsRetryBudget(t *testing.T) {
	state, lease, task := leaseState(core.SideEffectNone, core.ExecutionRunning, 2, 2)
	s := &Server{cfg: config.Server{EventRetention: 100}}
	s.expireLeaseLocked(&state, lease, time.Now().UTC(), "test disconnect")
	if task.Status != core.TaskDeadLetter {
		t.Fatalf("expected dead_letter, got %s", task.Status)
	}
}

func TestRetryDelayHasNoHourCapAndDoesNotOverflow(t *testing.T) {
	task := &core.Task{Attempts: 12, Spec: core.TaskSpec{RetryBackoffSeconds: 5}}
	if got, want := retryDelay(task), 10240*time.Second; got != want {
		t.Fatalf("retry delay = %s, want %s", got, want)
	}
	task.Attempts = int(^uint(0) >> 1)
	if got := retryDelay(task); got <= 0 {
		t.Fatalf("saturated retry delay overflowed: %s", got)
	}
}

func TestNonIdempotentPreparationFailureCanRetryBeforeStart(t *testing.T) {
	state, lease, task := leaseState(core.SideEffectNonIdempotent, core.ExecutionLeased, 1, 3)
	task.Spec.RetryOnFailure = true
	execution := state.Executions[lease.ExecutionID]
	applyFailureLocked(&state, task, lease, execution, time.Now().UTC(), -1, "input download failed", nil)
	if task.Status != core.TaskRetryWait {
		t.Fatalf("expected safe pre-start retry, got %s", task.Status)
	}
	if task.NextEligibleAt == nil {
		t.Fatal("pre-start retry has no backoff deadline")
	}
}

func TestNonIdempotentStartedFailureIsNotAutomaticallyRetried(t *testing.T) {
	state, lease, task := leaseState(core.SideEffectNonIdempotent, core.ExecutionRunning, 1, 3)
	task.Spec.RetryOnFailure = true
	execution := state.Executions[lease.ExecutionID]
	applyFailureLocked(&state, task, lease, execution, time.Now().UTC(), 1, "external action failed", nil)
	if task.Status != core.TaskFailed {
		t.Fatalf("expected failed pending explicit replay decision, got %s", task.Status)
	}
	if task.NextEligibleAt != nil {
		t.Fatal("started non-idempotent failure was automatically scheduled")
	}
}
