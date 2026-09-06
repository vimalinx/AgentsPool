package server

import (
	"testing"
	"time"

	"github.com/vimalinx/AgentsPool/internal/core"
)

func TestResourceLeaseWithoutExpiryRemainsActiveAndReserved(t *testing.T) {
	now := time.Now().UTC()
	lease := &core.ResourceLease{
		ID: "rlease_unbounded", NodeID: "node_unbounded", Status: core.ResourceLeaseActive,
		Resources: core.ResourceRequest{CPUMillis: 500, MemoryMB: 512, DiskMB: 256},
	}
	if !lease.ActiveAt(now) || !lease.ActiveAt(now.Add(100*365*24*time.Hour)) {
		t.Fatal("resource lease without expires_at must remain active")
	}
	state := core.NewState()
	state.ResourceLeases[lease.ID] = lease
	reserved := activeResourceReservation(&state, lease.NodeID, now.Add(100*365*24*time.Hour), "")
	if reserved != lease.Resources {
		t.Fatalf("unbounded lease reservation = %#v, want %#v", reserved, lease.Resources)
	}
}

func TestSandboxExecAllowsNoDeadlineAndNoPolicyCeiling(t *testing.T) {
	for _, timeoutSeconds := range []int{0, 86401, 7*24*3600 + 1} {
		spec := core.SandboxExecSpec{Command: []string{"/bin/true"}, TimeoutSeconds: timeoutSeconds}
		if err := validateSandboxExec(&spec); err != nil {
			t.Fatalf("timeout_seconds=%d rejected: %v", timeoutSeconds, err)
		}
		if spec.TimeoutSeconds != timeoutSeconds {
			t.Fatalf("timeout_seconds=%d normalized to %d", timeoutSeconds, spec.TimeoutSeconds)
		}
	}
}

func TestTaskAllowsNoDeadlineAndNoPolicyCeiling(t *testing.T) {
	for _, timeoutSeconds := range []int{0, 7*24*3600 + 1} {
		spec := core.TaskSpec{
			Name: "unbounded task", TimeoutSeconds: timeoutSeconds,
			Executor: core.ExecutorSpec{Kind: core.ExecutorShell, Script: "true"},
		}
		if err := normalizeTaskSpec(&spec); err != nil {
			t.Fatalf("timeout_seconds=%d rejected: %v", timeoutSeconds, err)
		}
		if spec.TimeoutSeconds != timeoutSeconds {
			t.Fatalf("timeout_seconds=%d normalized to %d", timeoutSeconds, spec.TimeoutSeconds)
		}
	}
}
