package server

import (
	"github.com/qiyuehuajin/agentspool/internal/core"
	"github.com/qiyuehuajin/agentspool/internal/store"
	"net/http"
	"testing"
	"time"
)

func seedLifecycle(t *testing.T, st *store.Store, running bool) {
	t.Helper()
	err := st.Update(func(s *core.State) error {
		now := time.Now().UTC()
		s.Nodes["node_review"] = &core.Node{ID: "node_review", Status: core.NodeOnline, TokenHash: core.HashSecret("review-worker-token"), LastSeenAt: now, Capabilities: core.NodeCapabilities{CPUThreads: 4, MemoryTotalMB: 4096, DiskTotalMB: 4096}, Resources: core.NodeResources{MemoryAvailableMB: 4096, DiskAvailableMB: 4096}}
		s.ResourceLeases["rlease_review"] = &core.ResourceLease{ID: "rlease_review", NodeID: "node_review", SandboxID: "sbx_review", Status: core.ResourceLeaseActive, Resources: core.ResourceRequest{CPUMillis: 1000, MemoryMB: 1024, DiskMB: 128}, CreatedAt: now}
		s.Sandboxes["sbx_review"] = &core.Sandbox{ID: "sbx_review", NodeID: "node_review", ResourceLeaseID: "rlease_review", Status: core.SandboxReady, Permissions: core.SandboxPermissions{Exec: true, DataRead: true, DataWrite: true}}
		if running {
			s.SandboxOperations["sbxop_running"] = &core.SandboxOperation{ID: "sbxop_running", NodeID: "node_review", SandboxID: "sbx_review", Kind: "exec", Status: core.SandboxOperationRunning, Exec: &core.SandboxExecSpec{Command: []string{"sleep", "600"}}, CreatedAt: now, ClaimTokenHash: core.HashSecret("running-token")}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
func claimLifecycle(t *testing.T, endpoint string, c *http.Client) sandboxOperationClaimResponse {
	t.Helper()
	var result sandboxOperationClaimResponse
	doTestRequest(t, c, http.MethodPost, endpoint+"/api/v1/workers/node_review/sandbox-operations/claim", struct{}{}, map[string]string{"Authorization": "Bearer review-worker-token"}, 200, &result)
	return result
}
func TestLifecycleRenewWithoutTTL(t *testing.T) {
	ts, st := newIntegrationServer(t)
	defer ts.Close()
	seedLifecycle(t, st, false)
	old := time.Now().UTC().Add(20 * time.Second)
	st.Update(func(s *core.State) error {
		l := s.ResourceLeases["rlease_review"]
		l.TTLSeconds = 60
		l.ExpiresAt = &old
		return nil
	})
	var out struct {
		Lease core.ResourceLease `json:"lease"`
	}
	doTestRequest(t, ts.Client(), http.MethodPost, ts.URL+"/api/v1/resource-leases/rlease_review/renew", struct{}{}, adminHeader(), 200, &out)
	if !out.Lease.ExpiresAt.After(old) {
		t.Fatalf("renew returned 200 but expiration unchanged: %s", out.Lease.ExpiresAt)
	}
}
func TestLifecycleDrainBlocksNewSandboxClaim(t *testing.T) {
	ts, st := newIntegrationServer(t)
	defer ts.Close()
	seedLifecycle(t, st, false)
	var out any
	doTestRequest(t, ts.Client(), http.MethodPost, ts.URL+"/api/v1/sandboxes/sbx_review/exec", map[string]any{"command": []string{"true"}}, adminHeader(), 202, &out)
	doTestRequest(t, ts.Client(), http.MethodPost, ts.URL+"/api/v1/nodes/node_review/drain", struct{}{}, adminHeader(), 200, &out)
	claim := claimLifecycle(t, ts.URL, ts.Client())
	if claim.Claimed {
		t.Fatalf("draining node still claimed new %s operation", claim.Operation.Kind)
	}
}
func TestLifecycleStopWaitsForRunningExec(t *testing.T) {
	ts, st := newIntegrationServer(t)
	defer ts.Close()
	seedLifecycle(t, st, true)
	var out any
	doTestRequest(t, ts.Client(), http.MethodPost, ts.URL+"/api/v1/sandboxes/sbx_review/stop", struct{}{}, adminHeader(), 202, &out)
	claim := claimLifecycle(t, ts.URL, ts.Client())
	if !claim.Claimed {
		return
	}
	headers := map[string]string{"Authorization": "Bearer review-worker-token", "X-AgentsPool-Sandbox-Operation-Token": claim.OperationToken}
	doTestRequest(t, ts.Client(), http.MethodPost, ts.URL+"/api/v1/sandbox-operations/"+claim.Operation.ID+"/complete", map[string]any{"success": true}, headers, 200, &out)
	s, _ := st.Snapshot()
	if s.Sandboxes["sbx_review"].Status == core.SandboxStopped && !s.SandboxOperations["sbxop_running"].Terminal() {
		t.Fatal("sandbox reports stopped while its exec remains running; no cancel acknowledgement was required")
	}
}
func TestLifecycleResizeDoesNotFreeRunningMemoryEarly(t *testing.T) {
	ts, st := newIntegrationServer(t)
	defer ts.Close()
	seedLifecycle(t, st, true)
	s, _ := st.Snapshot()
	var out any
	doTestRequest(t, ts.Client(), http.MethodPost, ts.URL+"/api/v1/sandboxes/sbx_review/resize", map[string]any{"expected_revision": s.RegistryRevision, "resources": core.ResourceRequest{CPUMillis: 100, MemoryMB: 128, DiskMB: 128}}, adminHeader(), 409, &out)
	claim := claimLifecycle(t, ts.URL, ts.Client())
	s, _ = st.Snapshot()
	if claim.Claimed && s.ResourceLeases["rlease_review"].Resources.MemoryMB == 128 && !s.SandboxOperations["sbxop_running"].Terminal() {
		t.Fatal("running exec originally admitted at 1024MB; reservation reduced to 128MB and resize claimed concurrently")
	}
}

func TestLifecycleResizeCommitsOnlyAfterHelperSuccess(t *testing.T) {
	for _, success := range []bool{false, true} {
		t.Run(map[bool]string{false: "failure", true: "success"}[success], func(t *testing.T) {
			ts, st := newIntegrationServer(t)
			defer ts.Close()
			seedLifecycle(t, st, false)
			state, _ := st.Snapshot()
			var out any
			desired := core.ResourceRequest{CPUMillis: 500, MemoryMB: 512, DiskMB: 256}
			doTestRequest(t, ts.Client(), http.MethodPost, ts.URL+"/api/v1/sandboxes/sbx_review/resize", map[string]any{"expected_revision": state.RegistryRevision, "resources": desired}, adminHeader(), 202, &out)
			state, _ = st.Snapshot()
			if got := state.ResourceLeases["rlease_review"].Resources; got.MemoryMB != 1024 || got.DiskMB != 128 {
				t.Fatalf("changed before completion: %+v", got)
			}
			reserved := activeResourceReservation(&state, "node_review", time.Now(), "")
			if reserved.MemoryMB != 1024 || reserved.DiskMB != 256 {
				t.Fatalf("pending reservation: %+v", reserved)
			}
			claim := claimLifecycle(t, ts.URL, ts.Client())
			if !claim.Claimed || *claim.Resources != desired {
				t.Fatalf("claim resources: %+v", claim.Resources)
			}
			headers := map[string]string{"Authorization": "Bearer review-worker-token", "X-AgentsPool-Sandbox-Operation-Token": claim.OperationToken}
			doTestRequest(t, ts.Client(), http.MethodPost, ts.URL+"/api/v1/sandbox-operations/"+claim.Operation.ID+"/complete", map[string]any{"success": success, "error": "synthetic helper result"}, headers, 200, &out)
			state, _ = st.Snapshot()
			got := state.ResourceLeases["rlease_review"].Resources
			if success && got != desired {
				t.Fatalf("successful resize not committed: %+v", got)
			}
			if !success && (got.MemoryMB != 1024 || got.DiskMB != 256) {
				t.Fatalf("failed grow lost conservative reservation: %+v", got)
			}
		})
	}
}

func TestLifecycleStoppedSandboxMustReadmitBeforeStart(t *testing.T) {
	ts, st := newIntegrationServer(t)
	defer ts.Close()
	seedLifecycle(t, st, false)
	st.Update(func(state *core.State) error {
		state.Sandboxes["sbx_review"].Status = core.SandboxStopped
		state.ResourceLeases["rlease_review"].ComputeReleased = true
		state.ResourceLeases["rlease_other"] = &core.ResourceLease{ID: "rlease_other", NodeID: "node_review", Status: core.ResourceLeaseActive, Resources: core.ResourceRequest{CPUMillis: 4000, MemoryMB: 4000, DiskMB: 128}}
		return nil
	})
	var out any
	doTestRequest(t, ts.Client(), http.MethodPost, ts.URL+"/api/v1/sandboxes/sbx_review/start", struct{}{}, adminHeader(), 409, &out)
	state, _ := st.Snapshot()
	if !state.ResourceLeases["rlease_review"].ComputeReleased {
		t.Fatal("failed start reacquired capacity")
	}
	st.Update(func(state *core.State) error { delete(state.ResourceLeases, "rlease_other"); return nil })
	doTestRequest(t, ts.Client(), http.MethodPost, ts.URL+"/api/v1/sandboxes/sbx_review/start", struct{}{}, adminHeader(), 202, &out)
	state, _ = st.Snapshot()
	if state.ResourceLeases["rlease_review"].ComputeReleased {
		t.Fatal("successful start did not reserve capacity")
	}
}

func TestLifecycleRestartDoesNotRequireASecondDiskCopy(t *testing.T) {
	ts, st := newIntegrationServer(t)
	defer ts.Close()
	seedLifecycle(t, st, false)
	st.Update(func(state *core.State) error {
		state.Sandboxes["sbx_review"].Status = core.SandboxStopped
		state.ResourceLeases["rlease_review"].ComputeReleased = true
		state.Nodes["node_review"].Resources.DiskAvailableMB = 0
		return nil
	})
	var out any
	doTestRequest(t, ts.Client(), http.MethodPost, ts.URL+"/api/v1/sandboxes/sbx_review/start", struct{}{}, adminHeader(), 202, &out)
}
