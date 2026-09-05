package worker

import (
	"context"
	"github.com/qiyuehuajin/agentspool/internal/client"
	"github.com/qiyuehuajin/agentspool/internal/config"
	"github.com/qiyuehuajin/agentspool/internal/core"
	"github.com/qiyuehuajin/agentspool/internal/sandboxd"
	"github.com/qiyuehuajin/agentspool/internal/server"
	"github.com/qiyuehuajin/agentspool/internal/store"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type reviewDeadlineHelper struct {
	started chan struct{}
	ended   chan error
}

func (h *reviewDeadlineHelper) ping(context.Context) error           { return nil }
func (h *reviewDeadlineHelper) cancel(context.Context, string) error { return nil }
func (h *reviewDeadlineHelper) status(context.Context, string) (sandboxd.OperationStatus, error) {
	return sandboxd.OperationStatus{}, nil
}
func (h *reviewDeadlineHelper) apply(ctx context.Context, _ sandboxd.OperationRequest) (sandboxd.OperationResult, error) {
	close(h.started)
	<-ctx.Done()
	h.ended <- ctx.Err()
	return sandboxd.OperationResult{}, ctx.Err()
}
func TestRenewalReachesRunningWorker(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	control, err := server.New(config.Server{ListenAddress: ":0", DataDir: st.DataDir(), LeaseTTL: 30 * time.Second, NodeTimeout: 20 * time.Second, ReaperInterval: time.Second, AdminToken: "review-admin-01234567890123456789", AllocatorToken: "review-allocator-01234567890123456789", JoinToken: "review-join-01234567890123456789", MaxRequestBytes: 2 << 20}, st, logger)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(control.Handler())
	defer ts.Close()
	expires := time.Now().UTC().Add(500 * time.Millisecond)
	resources := core.ResourceRequest{CPUMillis: 100, MemoryMB: 128, DiskMB: 64}
	sandbox := core.Sandbox{ID: "sbx_review", NodeID: "node_review", ResourceLeaseID: "rlease_review", Status: core.SandboxReady}
	operation := core.SandboxOperation{ID: "sbxop_review", SandboxID: sandbox.ID, NodeID: sandbox.NodeID, Kind: "exec", Status: core.SandboxOperationClaimed, ClaimTokenHash: core.HashSecret("review-operation"), Exec: &core.SandboxExecSpec{Command: []string{"sleep", "100"}}}
	err = st.Update(func(s *core.State) error {
		s.Nodes[sandbox.NodeID] = &core.Node{ID: sandbox.NodeID, Status: core.NodeOnline, TokenHash: core.HashSecret("review-node")}
		s.ResourceLeases[sandbox.ResourceLeaseID] = &core.ResourceLease{ID: sandbox.ResourceLeaseID, NodeID: sandbox.NodeID, SandboxID: sandbox.ID, Status: core.ResourceLeaseActive, ExpiresAt: &expires, TTLSeconds: 1, Resources: resources}
		s.Sandboxes[sandbox.ID] = &sandbox
		s.SandboxOperations[operation.ID] = &operation
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	api, _ := client.New(ts.URL, "review-node", time.Second, false)
	admin, _ := client.New(ts.URL, "review-admin-01234567890123456789", time.Second, false)
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	helper := &reviewDeadlineHelper{started: make(chan struct{}), ended: make(chan error, 1)}
	w := &Worker{cfg: config.Worker{RequestTimeout: time.Second, PollInterval: time.Millisecond}, api: api, logger: logger, sandbox: helper, sandboxActive: map[string]*sandboxOperationHandle{}, jobParent: parent, fatalCh: make(chan error, 1)}
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		w.runSandboxOperation(parent, sandboxOperationClaim{Operation: &operation, Sandbox: &sandbox, Resources: &resources, OperationToken: "review-operation", ResourceLeaseExpires: &expires})
	}()
	<-helper.started
	err = admin.DoJSON(context.Background(), http.MethodPost, "/api/v1/resource-leases/rlease_review/renew", nil, map[string]int{"ttl_seconds": 0}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, _ := st.Snapshot()
	if snapshot.ResourceLeases[sandbox.ResourceLeaseID].ExpiresAt != nil {
		t.Fatal("lease not converted to unbounded")
	}
	select {
	case err := <-helper.ended:
		<-finished
		t.Fatalf("control plane lease now has no expiry, but running worker ended at old deadline: %v", err)
	case <-time.After(time.Second):
		cancel()
		<-finished
	}
}

func TestLeaseRefreshNetworkStallDoesNotExtendConfirmedExpiry(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer ts.Close()
	api, err := client.New(ts.URL, "test-node", 5*time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultWorker()
	cfg.RequestTimeout = 5 * time.Second
	cfg.PollInterval = time.Second
	worker := &Worker{api: api, cfg: cfg}
	expiry := time.Now().Add(100 * time.Millisecond)
	claim := sandboxOperationClaim{Operation: &core.SandboxOperation{ID: "sbxop_stall", NodeID: "node_stall"}, ResourceLeaseExpires: &expiry, OperationToken: "test-operation"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); worker.monitorSandboxLease(ctx, cancel, claim) }()
	select {
	case <-done:
		if ctx.Err() == nil {
			t.Fatal("lease did not cancel execution")
		}
	case <-time.After(time.Second):
		t.Fatal("refresh extended finite lease through request timeout")
	}
}
