package server

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vimalinx/AgentsPool/internal/config"
	"github.com/vimalinx/AgentsPool/internal/core"
	"github.com/vimalinx/AgentsPool/internal/store"
)

const (
	testAdminToken     = "admin-token-0123456789-0123456789"
	testAllocatorToken = "allocator-token-0123456789-0123456789"
	testJoinToken      = "join-token-0123456789-01234567890"
)

func newIntegrationServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	stateStore, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Server{
		ListenAddress: ":0", DataDir: stateStore.DataDir(), AdminToken: testAdminToken, AllocatorToken: testAllocatorToken, JoinToken: testJoinToken,
		LeaseTTL: 30 * time.Second, NodeTimeout: 20 * time.Second, ReaperInterval: time.Second,
		MaxRequestBytes: 2 << 20, MaxArtifactBytes: 8 << 20, EventRetention: 1000,
	}
	s, err := New(cfg, stateStore, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewServer(s.Handler()), stateStore
}

func doTestRequest(t *testing.T, client *http.Client, method, endpoint string, body any, headers map[string]string, wantStatus int, out any) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, endpoint, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != wantStatus {
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("%s %s returned %d, want %d: %s", method, endpoint, resp.StatusCode, wantStatus, data)
	}
	if out != nil {
		defer resp.Body.Close()
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
	return resp
}

func adminHeader() map[string]string {
	return map[string]string{"Authorization": "Bearer " + testAdminToken}
}

func allocatorHeader() map[string]string {
	return map[string]string{"Authorization": "Bearer " + testAllocatorToken}
}

func TestTaskAPIRejectsGPUDispatchFields(t *testing.T) {
	ts, _ := newIntegrationServer(t)
	defer ts.Close()
	client := ts.Client()

	for name, spec := range map[string]map[string]any{
		"GPU requirement": {
			"name":         "must not schedule by GPU",
			"requirements": map[string]any{"gpu_count": 1},
			"executor":     map[string]any{"kind": "shell", "script": "true"},
		},
		"GPU executor": {
			"name":     "must not allocate GPU",
			"executor": map[string]any{"kind": "docker", "image": "example.invalid/test", "gpu": "all"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			doTestRequest(t, client, http.MethodPost, ts.URL+"/api/v1/tasks", spec, adminHeader(), http.StatusBadRequest, &APIError{})
		})
	}
}

func TestIdleWorkerPollingDoesNotPersistOrAdvanceRegistry(t *testing.T) {
	ts, stateStore := newIntegrationServer(t)
	defer ts.Close()
	client := ts.Client()
	var registration struct {
		Node struct {
			ID string `json:"id"`
		} `json:"node"`
		NodeToken string `json:"node_token"`
	}
	doTestRequest(t, client, http.MethodPost, ts.URL+"/api/v1/workers/register", map[string]any{
		"machine_id": "idle-machine", "name": "idle-worker",
		"capabilities": map[string]any{"hostname": "idle", "os": "linux", "arch": "amd64", "cpu_threads": 2, "memory_total_mb": 2048, "disk_total_mb": 10000, "capabilities": []string{"executor.shell"}, "queues": []string{"default"}, "agent_version": config.Version},
		"resources":    map[string]any{"memory_available_mb": 1500, "disk_available_mb": 9000, "max_concurrency": 1},
	}, map[string]string{"X-AgentsPool-Join-Token": testJoinToken}, http.StatusCreated, &registration)
	statePath := filepath.Join(stateStore.DataDir(), "state.json")
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var revisionBefore int64
	var eventsBefore int
	_ = stateStore.View(func(state *core.State) error {
		revisionBefore = state.RegistryRevision
		eventsBefore = len(state.RegistryEvents)
		return nil
	})
	nodeHeaders := map[string]string{"Authorization": "Bearer " + registration.NodeToken}
	doTestRequest(t, client, http.MethodPost, ts.URL+"/api/v1/workers/"+registration.Node.ID+"/heartbeat", map[string]any{
		"resources": map[string]any{"memory_available_mb": 1400, "disk_available_mb": 8900, "max_concurrency": 1},
	}, nodeHeaders, http.StatusOK, &map[string]any{})
	doTestRequest(t, client, http.MethodPost, ts.URL+"/api/v1/workers/"+registration.Node.ID+"/claim", ClaimRequest{
		Resources: core.NodeResources{MemoryAvailableMB: 1400, DiskAvailableMB: 8900, MaxConcurrency: 1},
	}, nodeHeaders, http.StatusOK, &ClaimResponse{})
	doTestRequest(t, client, http.MethodPost, ts.URL+"/api/v1/workers/"+registration.Node.ID+"/sandbox-operations/claim", struct{}{}, nodeHeaders, http.StatusOK, &sandboxOperationClaimResponse{})
	after, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("idle heartbeat or claim rewrote persistent state")
	}
	_ = stateStore.View(func(state *core.State) error {
		if state.RegistryRevision != revisionBefore || len(state.RegistryEvents) != eventsBefore {
			t.Fatalf("idle observation advanced registry: revision=%d events=%d", state.RegistryRevision, len(state.RegistryEvents))
		}
		if state.Nodes[registration.Node.ID].Resources.MemoryAvailableMB != 1400 {
			t.Fatal("volatile heartbeat telemetry was not kept in memory")
		}
		return nil
	})
}

func TestDeviceDescriptionsKeepHumanIntentSeparateFromAgentAdvice(t *testing.T) {
	ts, stateStore := newIntegrationServer(t)
	defer ts.Close()
	client := ts.Client()

	var registration struct {
		Node struct {
			ID string `json:"id"`
		} `json:"node"`
		NodeToken string `json:"node_token"`
	}
	registerBody := map[string]any{
		"machine_id": "described-machine", "name": "described-worker",
		"capabilities": map[string]any{
			"hostname": "described-host", "os": "linux", "arch": "amd64", "cpu_threads": 8,
			"memory_total_mb": 16384, "disk_total_mb": 100000,
			"capabilities": []string{"executor.shell"}, "labels": map[string]string{"role": "server"},
			"queues": []string{"default"}, "agent_version": config.Version,
		},
		"resources": map[string]any{"memory_available_mb": 12000, "disk_available_mb": 90000, "max_concurrency": 2},
	}
	doTestRequest(t, client, http.MethodPost, ts.URL+"/api/v1/workers/register", registerBody, map[string]string{"X-AgentsPool-Join-Token": testJoinToken}, http.StatusCreated, &registration)

	var listed struct {
		Revision int64            `json:"revision"`
		Devices  []deviceSnapshot `json:"devices"`
	}
	doTestRequest(t, client, http.MethodGet, ts.URL+"/api/v1/devices", nil, allocatorHeader(), http.StatusOK, &listed)
	if len(listed.Devices) != 1 || len(listed.Devices[0].Descriptions) != 0 {
		t.Fatalf("unexpected initial device descriptions: %#v", listed)
	}

	human := map[string]any{
		"expected_revision": listed.Revision, "source": "human", "author": "owner",
		"description":  "Always-on machine for durable LAN services.",
		"availability": "always_on", "service_policy": "preferred",
		"preferred_workloads": []string{"service", "backup", "service"},
		"avoid_workloads":     []string{"interactive-desktop"},
		"constraints":         []string{"Do not interrupt existing services."},
	}
	doTestRequest(t, client, http.MethodPut, ts.URL+"/api/v1/devices/"+registration.Node.ID+"/descriptions", human, allocatorHeader(), http.StatusForbidden, &APIError{})
	var humanResponse struct {
		Created  bool           `json:"created"`
		Revision int64          `json:"revision"`
		Device   deviceSnapshot `json:"device"`
	}
	doTestRequest(t, client, http.MethodPut, ts.URL+"/api/v1/devices/"+registration.Node.ID+"/descriptions", human, adminHeader(), http.StatusOK, &humanResponse)
	if !humanResponse.Created || humanResponse.Revision <= listed.Revision || len(humanResponse.Device.Descriptions) != 1 {
		t.Fatalf("unexpected human description response: %#v", humanResponse)
	}
	humanStored := humanResponse.Device.Descriptions[0]
	if humanStored.Source != core.DeviceDescriptionSourceHuman || humanStored.Availability != core.DeviceAvailabilityAlwaysOn || humanStored.ServicePolicy != core.DeviceServicePolicyPreferred || len(humanStored.PreferredWorkloads) != 2 || humanStored.BasisRevision != listed.Revision {
		t.Fatalf("human description was not normalized: %#v", humanStored)
	}

	staleAgent := map[string]any{
		"expected_revision": listed.Revision, "source": "agent", "author": "planner",
		"description": "Suggested for builds.", "preferred_workloads": []string{"build"},
	}
	var staleError APIError
	doTestRequest(t, client, http.MethodPut, ts.URL+"/api/v1/devices/"+registration.Node.ID+"/descriptions", staleAgent, adminHeader(), http.StatusConflict, &staleError)
	if staleError.Error.Code != "stale_registry_revision" {
		t.Fatalf("unexpected stale description error: %#v", staleError)
	}

	agent := map[string]any{
		"expected_revision": humanResponse.Revision, "source": "agent", "author": "planner",
		"description":  "Hardware suggests build and media workloads.",
		"availability": "unknown", "service_policy": "allowed",
		"preferred_workloads": []string{"build", "media"},
		"constraints":         []string{"Human availability policy remains authoritative."},
	}
	var agentResponse struct {
		Revision int64          `json:"revision"`
		Device   deviceSnapshot `json:"device"`
	}
	doTestRequest(t, client, http.MethodPut, ts.URL+"/api/v1/devices/"+registration.Node.ID+"/descriptions", agent, allocatorHeader(), http.StatusOK, &agentResponse)
	if len(agentResponse.Device.Descriptions) != 2 || agentResponse.Device.Descriptions[0].Source != core.DeviceDescriptionSourceHuman || agentResponse.Device.Descriptions[1].Source != core.DeviceDescriptionSourceAgent {
		t.Fatalf("human description must remain separate and first: %#v", agentResponse.Device.Descriptions)
	}

	doTestRequest(t, client, http.MethodPost, ts.URL+"/api/v1/workers/"+registration.Node.ID+"/heartbeat", map[string]any{
		"resources": map[string]any{"memory_available_mb": 11000, "disk_available_mb": 88000, "max_concurrency": 2},
	}, map[string]string{"Authorization": "Bearer " + registration.NodeToken}, http.StatusOK, &map[string]any{})
	var afterHeartbeat struct {
		Revision int64            `json:"revision"`
		Devices  []deviceSnapshot `json:"devices"`
	}
	doTestRequest(t, client, http.MethodGet, ts.URL+"/api/v1/devices", nil, adminHeader(), http.StatusOK, &afterHeartbeat)
	if len(afterHeartbeat.Devices[0].Descriptions) != 2 {
		t.Fatalf("worker heartbeat erased control-plane descriptions: %#v", afterHeartbeat.Devices[0])
	}

	remove := map[string]any{"expected_revision": afterHeartbeat.Revision, "source": "agent", "author": "planner"}
	var removed struct {
		Removed  bool           `json:"removed"`
		Revision int64          `json:"revision"`
		Device   deviceSnapshot `json:"device"`
	}
	doTestRequest(t, client, http.MethodDelete, ts.URL+"/api/v1/devices/"+registration.Node.ID+"/descriptions", remove, adminHeader(), http.StatusOK, &removed)
	if !removed.Removed || len(removed.Device.Descriptions) != 1 || removed.Device.Descriptions[0].Source != core.DeviceDescriptionSourceHuman {
		t.Fatalf("removing Agent advice changed human intent: %#v", removed)
	}

	if err := stateStore.View(func(state *core.State) error {
		if len(state.Nodes[registration.Node.ID].Descriptions) != 1 {
			t.Fatalf("descriptions were not durably stored: %#v", state.Nodes[registration.Node.ID].Descriptions)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDeviceRegistryResourceLeaseAndSandboxControlFlow(t *testing.T) {
	ts, stateStore := newIntegrationServer(t)
	defer ts.Close()
	client := ts.Client()

	var registration struct {
		Node struct {
			ID string `json:"id"`
		} `json:"node"`
		NodeToken string `json:"node_token"`
	}
	registerBody := map[string]any{
		"machine_id": "sandbox-machine", "name": "sandbox-worker",
		"capabilities": map[string]any{
			"hostname": "test", "os": "linux", "arch": "amd64", "cpu_threads": 4,
			"memory_total_mb": 4096, "disk_total_mb": 20000,
			"capabilities": []string{"executor.shell", "sandbox.systemd-bwrap", "sandbox.persistent"},
			"queues":       []string{"default"}, "agent_version": config.Version,
		},
		"resources": map[string]any{"memory_available_mb": 3072, "disk_available_mb": 15000, "max_concurrency": 1},
	}
	doTestRequest(t, client, http.MethodPost, ts.URL+"/api/v1/workers/register", registerBody, map[string]string{"X-AgentsPool-Join-Token": testJoinToken}, http.StatusCreated, &registration)
	nodeHeaders := map[string]string{"Authorization": "Bearer " + registration.NodeToken}

	var devices struct {
		Revision int64            `json:"revision"`
		Devices  []deviceSnapshot `json:"devices"`
	}
	doTestRequest(t, client, http.MethodGet, ts.URL+"/api/v1/devices", nil, adminHeader(), http.StatusOK, &devices)
	if devices.Revision < 1 || len(devices.Devices) != 1 || devices.Devices[0].Available.CPUMillis != 4000 {
		t.Fatalf("unexpected device registry snapshot: %#v", devices)
	}

	acquire := resourceLeaseAcquireRequest{
		NodeID: registration.Node.ID, Owner: "integration-agent", ExpectedRevision: devices.Revision,
		Resources: core.ResourceRequest{CPUMillis: 750, MemoryMB: 512, DiskMB: 1024}, TTLSeconds: 300,
	}
	var acquired struct {
		Lease      core.ResourceLease `json:"lease"`
		LeaseToken string             `json:"lease_token"`
		Revision   int64              `json:"revision"`
	}
	doTestRequest(t, client, http.MethodPost, ts.URL+"/api/v1/resource-leases", acquire, allocatorHeader(), http.StatusCreated, &acquired)
	if acquired.Lease.ID == "" || acquired.LeaseToken == "" || acquired.Lease.TokenHash != "" || acquired.Revision <= devices.Revision {
		t.Fatalf("unexpected resource lease: %#v", acquired)
	}
	doTestRequest(t, client, http.MethodPost, ts.URL+"/api/v1/resource-leases", acquire, adminHeader(), http.StatusConflict, &APIError{})
	invalidGPU := map[string]any{
		"node_id": registration.Node.ID, "expected_revision": acquired.Revision,
		"resources": map[string]any{"cpu_millis": 100, "memory_mb": 64, "disk_mb": 10, "gpu_count": 1},
	}
	doTestRequest(t, client, http.MethodPost, ts.URL+"/api/v1/resource-leases", invalidGPU, adminHeader(), http.StatusBadRequest, &APIError{})

	leaseHeaders := map[string]string{"X-AgentsPool-Resource-Lease-Token": acquired.LeaseToken}
	noDeadline := 0
	var renewed struct {
		Lease core.ResourceLease `json:"lease"`
	}
	doTestRequest(t, client, http.MethodPost, ts.URL+"/api/v1/resource-leases/"+acquired.Lease.ID+"/renew", resourceLeaseRenewRequest{TTLSeconds: &noDeadline}, leaseHeaders, http.StatusOK, &renewed)
	if renewed.Lease.TTLSeconds != 0 || renewed.Lease.ExpiresAt != nil || !renewed.Lease.ActiveAt(time.Now().UTC().Add(100*365*24*time.Hour)) {
		t.Fatalf("resource lease was not converted to no deadline: %#v", renewed.Lease)
	}
	create := sandboxCreateRequest{
		Name: "persistent-integration", ResourceLeaseID: acquired.Lease.ID,
		Permissions: core.SandboxPermissions{Exec: true, DataRead: true, DataWrite: true, Network: true},
	}
	var created struct {
		Sandbox      core.Sandbox          `json:"sandbox"`
		SandboxToken string                `json:"sandbox_token"`
		Operation    core.SandboxOperation `json:"operation"`
	}
	doTestRequest(t, client, http.MethodPost, ts.URL+"/api/v1/sandboxes", create, leaseHeaders, http.StatusCreated, &created)
	if created.Sandbox.ID == "" || created.SandboxToken == "" || created.Operation.Kind != "ensure" || created.Sandbox.TokenHash != "" {
		t.Fatalf("unexpected sandbox creation: %#v", created)
	}

	claimOperation := func(wantKind string) sandboxOperationClaimResponse {
		t.Helper()
		var claim sandboxOperationClaimResponse
		doTestRequest(t, client, http.MethodPost, ts.URL+"/api/v1/workers/"+registration.Node.ID+"/sandbox-operations/claim", struct{}{}, nodeHeaders, http.StatusOK, &claim)
		if !claim.Claimed || claim.Operation == nil || claim.Operation.Kind != wantKind || claim.OperationToken == "" || claim.Resources == nil {
			t.Fatalf("unexpected %s claim: %#v", wantKind, claim)
		}
		return claim
	}
	completeOperation := func(claim sandboxOperationClaimResponse, result sandboxOperationCompleteRequest) {
		t.Helper()
		headers := map[string]string{
			"Authorization":                        "Bearer " + registration.NodeToken,
			"X-AgentsPool-Sandbox-Operation-Token": claim.OperationToken,
		}
		doTestRequest(t, client, http.MethodPost, ts.URL+"/api/v1/sandbox-operations/"+claim.Operation.ID+"/complete", result, headers, http.StatusOK, &map[string]any{})
	}
	startOperation := func(claim sandboxOperationClaimResponse) {
		t.Helper()
		headers := map[string]string{
			"Authorization":                        "Bearer " + registration.NodeToken,
			"X-AgentsPool-Sandbox-Operation-Token": claim.OperationToken,
		}
		doTestRequest(t, client, http.MethodPost, ts.URL+"/api/v1/sandbox-operations/"+claim.Operation.ID+"/start", struct{}{}, headers, http.StatusOK, &map[string]any{})
	}
	ensureClaim := claimOperation("ensure")
	if ensureClaim.ResourceLeaseExpires != nil {
		t.Fatalf("unbounded resource lease exposed an expiry: %v", ensureClaim.ResourceLeaseExpires)
	}
	completeOperation(ensureClaim, sandboxOperationCompleteRequest{Success: true})

	sandboxHeaders := map[string]string{"X-AgentsPool-Sandbox-Token": created.SandboxToken}
	doTestRequest(t, client, http.MethodGet, ts.URL+"/api/v1/devices", nil, sandboxHeaders, http.StatusUnauthorized, &APIError{})
	doTestRequest(t, client, http.MethodPost, ts.URL+"/api/v1/sandboxes/"+created.Sandbox.ID+"/exec", core.SandboxExecSpec{
		Command: []string{"/bin/true"}, WorkingDir: "/workspace/../../etc", TimeoutSeconds: 10,
	}, sandboxHeaders, http.StatusBadRequest, &APIError{})
	var execResponse struct {
		Operation core.SandboxOperation `json:"operation"`
	}
	healthPort := ts.Listener.Addr().(*net.TCPAddr).Port
	doTestRequest(t, client, http.MethodPost, ts.URL+"/api/v1/sandboxes/"+created.Sandbox.ID+"/exec", core.SandboxExecSpec{
		Command: []string{"/bin/sh", "-c", "printf persistent"}, TimeoutSeconds: 10,
		Endpoints: []core.SandboxEndpoint{{Name: "web", Protocol: "http", Port: healthPort, URL: ts.URL, HealthURL: ts.URL + "/healthz"}},
	}, sandboxHeaders, http.StatusAccepted, &execResponse)
	execClaim := claimOperation("exec")
	startOperation(execClaim)
	if err := stateStore.Update(func(state *core.State) error {
		state.SandboxOperations[execClaim.Operation.ID].Status = core.SandboxOperationOrphaned
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var recoveryHeartbeat struct {
		ReconcileSandboxOperationIDs []string `json:"reconcile_sandbox_operation_ids"`
	}
	doTestRequest(t, client, http.MethodPost, ts.URL+"/api/v1/workers/"+registration.Node.ID+"/heartbeat", map[string]any{
		"resources": map[string]any{"memory_available_mb": 3072, "disk_available_mb": 15000, "max_concurrency": 1},
	}, nodeHeaders, http.StatusOK, &recoveryHeartbeat)
	if len(recoveryHeartbeat.ReconcileSandboxOperationIDs) != 1 || recoveryHeartbeat.ReconcileSandboxOperationIDs[0] != execClaim.Operation.ID {
		t.Fatalf("worker did not receive orphan reconciliation request: %#v", recoveryHeartbeat)
	}
	doTestRequest(t, client, http.MethodPost, ts.URL+"/api/v1/workers/"+registration.Node.ID+"/sandbox-operations/"+execClaim.Operation.ID+"/reconcile", sandboxOperationReconcileRequest{Running: true, ActiveState: "active", SubState: "running"}, nodeHeaders, http.StatusOK, &map[string]any{})
	var services struct {
		Count    int                `json:"count"`
		Services []publishedService `json:"services"`
	}
	doTestRequest(t, client, http.MethodGet, ts.URL+"/api/v1/services", nil, allocatorHeader(), http.StatusOK, &services)
	if services.Count != 1 || len(services.Services) != 1 || services.Services[0].OperationID != execClaim.Operation.ID || services.Services[0].Endpoints[0].Port != healthPort || services.Services[0].Health != "healthy" {
		t.Fatalf("running service was not discoverable: %#v", services)
	}
	exitCode := 0
	completeOperation(execClaim, sandboxOperationCompleteRequest{Success: true, ExitCode: &exitCode, Stdout: "persistent"})
	doTestRequest(t, client, http.MethodGet, ts.URL+"/api/v1/services", nil, allocatorHeader(), http.StatusOK, &services)
	if services.Count != 0 || len(services.Services) != 0 {
		t.Fatalf("completed service remained published: %#v", services)
	}

	var operationDetail struct {
		Operation core.SandboxOperation `json:"operation"`
	}
	doTestRequest(t, client, http.MethodGet, ts.URL+"/api/v1/sandbox-operations/"+execResponse.Operation.ID, nil, sandboxHeaders, http.StatusOK, &operationDetail)
	if operationDetail.Operation.Status != core.SandboxOperationSucceeded || operationDetail.Operation.Stdout != "persistent" || operationDetail.Operation.ClaimTokenHash != "" {
		t.Fatalf("unexpected sandbox operation result: %#v", operationDetail)
	}

	pushRequest, err := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/sandboxes/"+created.Sandbox.ID+"/files?path=%2Fworkspace%2Finput.txt", strings.NewReader("from-agent"))
	if err != nil {
		t.Fatal(err)
	}
	pushRequest.Header.Set("X-AgentsPool-Sandbox-Token", created.SandboxToken)
	pushRequest.Header.Set("Content-Type", "text/plain")
	pushHTTPResponse, err := client.Do(pushRequest)
	if err != nil {
		t.Fatal(err)
	}
	if pushHTTPResponse.StatusCode != http.StatusAccepted {
		data, _ := io.ReadAll(pushHTTPResponse.Body)
		pushHTTPResponse.Body.Close()
		t.Fatalf("sandbox file push returned %d: %s", pushHTTPResponse.StatusCode, data)
	}
	var pushed struct {
		Artifact  core.Artifact         `json:"artifact"`
		Operation core.SandboxOperation `json:"operation"`
	}
	if err := json.NewDecoder(pushHTTPResponse.Body).Decode(&pushed); err != nil {
		t.Fatal(err)
	}
	pushHTTPResponse.Body.Close()
	if pushed.Artifact.Metadata["sandbox_id"] != created.Sandbox.ID || pushed.Operation.Kind != "put" {
		t.Fatalf("unexpected sandbox file push: %#v", pushed)
	}
	putClaim := claimOperation("put")
	completeOperation(putClaim, sandboxOperationCompleteRequest{Success: true})

	var exported struct {
		Operation core.SandboxOperation `json:"operation"`
	}
	doTestRequest(t, client, http.MethodPost, ts.URL+"/api/v1/sandboxes/"+created.Sandbox.ID+"/files/export", map[string]string{"path": "/workspace/input.txt"}, sandboxHeaders, http.StatusAccepted, &exported)
	getClaim := claimOperation("get")
	exportHeaders := map[string]string{
		"Authorization":                        "Bearer " + registration.NodeToken,
		"X-AgentsPool-Sandbox-Operation-Token": getClaim.OperationToken,
	}
	uploadExport, err := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/sandbox-operations/"+getClaim.Operation.ID+"/artifact", strings.NewReader("back-from-node"))
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range exportHeaders {
		uploadExport.Header.Set(key, value)
	}
	uploadExport.Header.Set("Content-Type", "text/plain")
	uploadExportResponse, err := client.Do(uploadExport)
	if err != nil {
		t.Fatal(err)
	}
	if uploadExportResponse.StatusCode != http.StatusCreated {
		data, _ := io.ReadAll(uploadExportResponse.Body)
		uploadExportResponse.Body.Close()
		t.Fatalf("sandbox export upload returned %d: %s", uploadExportResponse.StatusCode, data)
	}
	uploadExportResponse.Body.Close()
	completeOperation(getClaim, sandboxOperationCompleteRequest{Success: true})
	doTestRequest(t, client, http.MethodGet, ts.URL+"/api/v1/sandbox-operations/"+exported.Operation.ID, nil, sandboxHeaders, http.StatusOK, &operationDetail)
	if operationDetail.Operation.ResultArtifactID == "" {
		t.Fatalf("sandbox export did not attach an artifact: %#v", operationDetail.Operation)
	}
	downloadExport, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/sandboxes/"+created.Sandbox.ID+"/artifacts/"+operationDetail.Operation.ResultArtifactID+"/content", nil)
	downloadExport.Header.Set("X-AgentsPool-Sandbox-Token", created.SandboxToken)
	downloadExportResponse, err := client.Do(downloadExport)
	if err != nil {
		t.Fatal(err)
	}
	exportedData, _ := io.ReadAll(downloadExportResponse.Body)
	downloadExportResponse.Body.Close()
	if downloadExportResponse.StatusCode != http.StatusOK || string(exportedData) != "back-from-node" {
		t.Fatalf("sandbox scoped download failed: status=%d data=%q", downloadExportResponse.StatusCode, exportedData)
	}
	var sandboxArtifacts struct {
		Artifacts []core.Artifact `json:"artifacts"`
		Count     int             `json:"count"`
	}
	doTestRequest(t, client, http.MethodGet, ts.URL+"/api/v1/sandboxes/"+created.Sandbox.ID+"/artifacts", nil, sandboxHeaders, http.StatusOK, &sandboxArtifacts)
	if sandboxArtifacts.Count != 2 || len(sandboxArtifacts.Artifacts) != 2 {
		t.Fatalf("sandbox artifacts were not discoverable: %#v", sandboxArtifacts)
	}
	doTestRequest(t, client, http.MethodDelete, ts.URL+"/api/v1/sandboxes/"+created.Sandbox.ID+"/artifacts/"+operationDetail.Operation.ResultArtifactID, nil, sandboxHeaders, http.StatusOK, &map[string]any{})

	var longExec struct {
		Operation core.SandboxOperation `json:"operation"`
	}
	doTestRequest(t, client, http.MethodPost, ts.URL+"/api/v1/sandboxes/"+created.Sandbox.ID+"/exec", core.SandboxExecSpec{
		Command: []string{"/bin/sh", "-c", "while :; do sleep 60; done"}, TimeoutSeconds: 0,
	}, sandboxHeaders, http.StatusAccepted, &longExec)
	longClaim := claimOperation("exec")
	startOperation(longClaim)
	doTestRequest(t, client, http.MethodPost, ts.URL+"/api/v1/sandboxes/"+created.Sandbox.ID+"/stop", struct{}{}, sandboxHeaders, http.StatusAccepted, &map[string]any{})
	var heartbeat struct {
		CancelSandboxOperationIDs []string `json:"cancel_sandbox_operation_ids"`
	}
	doTestRequest(t, client, http.MethodPost, ts.URL+"/api/v1/workers/"+registration.Node.ID+"/heartbeat", map[string]any{
		"resources": map[string]any{"memory_available_mb": 3072, "disk_available_mb": 15000, "max_concurrency": 1},
	}, nodeHeaders, http.StatusOK, &heartbeat)
	if len(heartbeat.CancelSandboxOperationIDs) != 1 || heartbeat.CancelSandboxOperationIDs[0] != longExec.Operation.ID {
		t.Fatalf("worker did not receive sandbox cancellation: %#v", heartbeat)
	}
	completeOperation(longClaim, sandboxOperationCompleteRequest{Cancelled: true, Error: "cancelled by sandbox stop"})
	doTestRequest(t, client, http.MethodGet, ts.URL+"/api/v1/sandbox-operations/"+longExec.Operation.ID, nil, sandboxHeaders, http.StatusOK, &operationDetail)
	if operationDetail.Operation.Status != core.SandboxOperationCancelled || operationDetail.Operation.CancelledAt == nil {
		t.Fatalf("long sandbox operation was not cancelled: %#v", operationDetail.Operation)
	}
	stopClaim := claimOperation("stop")
	completeOperation(stopClaim, sandboxOperationCompleteRequest{Success: true})
	doTestRequest(t, client, http.MethodPost, ts.URL+"/api/v1/sandboxes/"+created.Sandbox.ID+"/exec", core.SandboxExecSpec{
		Command: []string{"/bin/true"}, TimeoutSeconds: 10,
	}, sandboxHeaders, http.StatusConflict, &APIError{})
	doTestRequest(t, client, http.MethodPost, ts.URL+"/api/v1/sandboxes/"+created.Sandbox.ID+"/start", struct{}{}, sandboxHeaders, http.StatusAccepted, &map[string]any{})
	startClaim := claimOperation("ensure")
	completeOperation(startClaim, sandboxOperationCompleteRequest{Success: true})

	var resizeDevices struct {
		Revision int64            `json:"revision"`
		Devices  []deviceSnapshot `json:"devices"`
	}
	doTestRequest(t, client, http.MethodGet, ts.URL+"/api/v1/devices", nil, adminHeader(), http.StatusOK, &resizeDevices)
	var resized struct {
		Operation core.SandboxOperation `json:"operation"`
	}
	doTestRequest(t, client, http.MethodPost, ts.URL+"/api/v1/sandboxes/"+created.Sandbox.ID+"/resize", sandboxResizeRequest{
		ExpectedRevision: resizeDevices.Revision,
		Resources:        core.ResourceRequest{CPUMillis: 600, MemoryMB: 384, DiskMB: 1536},
	}, sandboxHeaders, http.StatusAccepted, &resized)
	resizeClaim := claimOperation("resize")
	completeOperation(resizeClaim, sandboxOperationCompleteRequest{Success: true})

	var rebindDevices struct {
		Revision int64            `json:"revision"`
		Devices  []deviceSnapshot `json:"devices"`
	}
	doTestRequest(t, client, http.MethodGet, ts.URL+"/api/v1/devices", nil, adminHeader(), http.StatusOK, &rebindDevices)
	var replacement struct {
		Lease      core.ResourceLease `json:"lease"`
		LeaseToken string             `json:"lease_token"`
	}
	doTestRequest(t, client, http.MethodPost, ts.URL+"/api/v1/resource-leases", resourceLeaseAcquireRequest{
		NodeID: registration.Node.ID, Owner: "replacement-agent", ExpectedRevision: rebindDevices.Revision,
		Resources: core.ResourceRequest{CPUMillis: 500, MemoryMB: 256, DiskMB: 1536}, TTLSeconds: 300,
	}, allocatorHeader(), http.StatusCreated, &replacement)
	rebindHeaders := map[string]string{
		"X-AgentsPool-Sandbox-Token":        created.SandboxToken,
		"X-AgentsPool-Resource-Lease-Token": replacement.LeaseToken,
	}
	var rebound struct {
		Operation core.SandboxOperation `json:"operation"`
	}
	doTestRequest(t, client, http.MethodPost, ts.URL+"/api/v1/sandboxes/"+created.Sandbox.ID+"/rebind", sandboxRebindRequest{
		ResourceLeaseID: replacement.Lease.ID,
	}, rebindHeaders, http.StatusAccepted, &rebound)
	rebindClaim := claimOperation("ensure")
	completeOperation(rebindClaim, sandboxOperationCompleteRequest{Success: true})
	var reboundSandbox struct {
		Sandbox core.Sandbox `json:"sandbox"`
	}
	doTestRequest(t, client, http.MethodGet, ts.URL+"/api/v1/sandboxes/"+created.Sandbox.ID, nil, sandboxHeaders, http.StatusOK, &reboundSandbox)
	if reboundSandbox.Sandbox.ResourceLeaseID != replacement.Lease.ID {
		t.Fatalf("sandbox did not bind replacement lease: %#v", reboundSandbox.Sandbox)
	}
	doTestRequest(t, client, http.MethodDelete, ts.URL+"/api/v1/resource-leases/"+acquired.Lease.ID, nil, leaseHeaders, http.StatusOK, &map[string]any{})

	doTestRequest(t, client, http.MethodDelete, ts.URL+"/api/v1/sandboxes/"+created.Sandbox.ID, nil, sandboxHeaders, http.StatusAccepted, &map[string]any{})
	doTestRequest(t, client, http.MethodPost, ts.URL+"/api/v1/sandboxes/"+created.Sandbox.ID+"/exec", core.SandboxExecSpec{
		Command: []string{"/bin/true"}, TimeoutSeconds: 10,
	}, sandboxHeaders, http.StatusConflict, &APIError{})
	destroyClaim := claimOperation("destroy")
	completeOperation(destroyClaim, sandboxOperationCompleteRequest{Success: true})
	_ = stateStore.View(func(state *core.State) error {
		if len(state.Artifacts) != 0 {
			t.Fatalf("sandbox destroy left bridge artifacts: %#v", state.Artifacts)
		}
		return nil
	})
	replacementHeaders := map[string]string{"X-AgentsPool-Resource-Lease-Token": replacement.LeaseToken}
	doTestRequest(t, client, http.MethodDelete, ts.URL+"/api/v1/resource-leases/"+replacement.Lease.ID, nil, replacementHeaders, http.StatusOK, &map[string]any{})
	var preview maintenancePruneResult
	doTestRequest(t, client, http.MethodPost, ts.URL+"/api/v1/maintenance/prune", maintenancePruneRequest{}, adminHeader(), http.StatusOK, &preview)
	if !preview.DryRun || len(preview.SandboxIDs) != 1 || len(preview.SandboxOperationIDs) == 0 || len(preview.ResourceLeaseIDs) != 2 {
		t.Fatalf("unexpected maintenance preview: %#v", preview)
	}
	var pruned maintenancePruneResult
	doTestRequest(t, client, http.MethodPost, ts.URL+"/api/v1/maintenance/prune", maintenancePruneRequest{Execute: true}, adminHeader(), http.StatusOK, &pruned)
	if pruned.DryRun || pruned.SandboxesDeleted != 1 || pruned.ResourceLeasesDeleted != 2 {
		t.Fatalf("unexpected maintenance result: %#v", pruned)
	}
	if err := stateStore.View(func(state *core.State) error {
		if len(state.Sandboxes) != 0 || len(state.ResourceLeases) != 0 || len(state.SandboxOperations) != 0 || len(state.SandboxVolumes) != 0 || len(state.Artifacts) != 0 {
			t.Fatalf("maintenance prune left terminal sandbox state: %#v", state)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestControlPlaneWorkerEndToEnd(t *testing.T) {
	ts, stateStore := newIntegrationServer(t)
	defer ts.Close()
	client := ts.Client()

	var registration struct {
		Node struct {
			ID string `json:"id"`
		} `json:"node"`
		NodeToken string `json:"node_token"`
	}
	registerBody := map[string]any{
		"machine_id": "integration-machine", "name": "integration-worker",
		"capabilities": map[string]any{
			"hostname": "test", "os": "linux", "arch": "amd64", "cpu_threads": 8,
			"memory_total_mb": 16000, "disk_total_mb": 100000,
			"capabilities": []string{"executor.shell", "executor.process"}, "queues": []string{"default"}, "agent_version": config.Version,
		},
		"resources": map[string]any{
			"memory_available_mb": 12000, "disk_available_mb": 90000, "active_jobs": 0, "max_concurrency": 2,
		},
	}
	doTestRequest(t, client, http.MethodPost, ts.URL+"/api/v1/workers/register", registerBody, map[string]string{"X-AgentsPool-Join-Token": testJoinToken}, http.StatusCreated, &registration)
	if registration.Node.ID == "" || registration.NodeToken == "" {
		t.Fatalf("incomplete registration response: %#v", registration)
	}
	nodeHeaders := map[string]string{"Authorization": "Bearer " + registration.NodeToken}

	spec := core.TaskSpec{
		Name: "integration shell job", IdempotencyKey: "integration-idempotency-key",
		Executor: core.ExecutorSpec{Kind: core.ExecutorShell, Script: "echo integration"},
		Outputs:  []core.OutputRule{{Pattern: "outputs/**/*.txt", Required: true}},
	}
	var submitted taskSubmitResponse
	doTestRequest(t, client, http.MethodPost, ts.URL+"/api/v1/tasks", spec, adminHeader(), http.StatusCreated, &submitted)
	if submitted.Task == nil || !submitted.Created || submitted.Task.Status != core.TaskQueued {
		t.Fatalf("unexpected submit response: %#v", submitted)
	}
	var duplicate taskSubmitResponse
	doTestRequest(t, client, http.MethodPost, ts.URL+"/api/v1/tasks", spec, adminHeader(), http.StatusOK, &duplicate)
	if duplicate.Created || duplicate.Task.ID != submitted.Task.ID {
		t.Fatalf("idempotency did not return original task: %#v", duplicate)
	}

	claimBody := ClaimRequest{Resources: core.NodeResources{MemoryAvailableMB: 12000, DiskAvailableMB: 90000, MaxConcurrency: 2}}
	var claim ClaimResponse
	doTestRequest(t, client, http.MethodPost, ts.URL+"/api/v1/workers/"+registration.Node.ID+"/claim", claimBody, nodeHeaders, http.StatusOK, &claim)
	if !claim.Claimed || claim.Task == nil || claim.LeaseID == "" || claim.LeaseToken == "" || claim.ExecutionID == "" {
		t.Fatalf("unexpected claim response: %#v", claim)
	}
	leaseHeaders := map[string]string{
		"Authorization":            "Bearer " + registration.NodeToken,
		"X-AgentsPool-Lease-Token": claim.LeaseToken,
	}
	doTestRequest(t, client, http.MethodPost, ts.URL+"/api/v1/leases/"+claim.LeaseID+"/start", struct{}{}, leaseHeaders, http.StatusOK, &map[string]any{})

	logBody := leaseLogRequest{Events: []store.LogEvent{{Sequence: 1, Stream: "stdout", Data: "integration output\n"}}}
	doTestRequest(t, client, http.MethodPost, ts.URL+"/api/v1/leases/"+claim.LeaseID+"/logs", logBody, leaseHeaders, http.StatusAccepted, &map[string]any{})

	artifactURL := ts.URL + "/api/v1/leases/" + claim.LeaseID + "/artifacts?" + url.Values{
		"name":          {"result.txt"},
		"media_type":    {"text/plain"},
		"relative_path": {"outputs/nested/result.txt"},
	}.Encode()
	req, err := http.NewRequest(http.MethodPost, artifactURL, strings.NewReader("artifact-ok"))
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range leaseHeaders {
		req.Header.Set(key, value)
	}
	req.Header.Set("Content-Type", "text/plain")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusCreated {
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("artifact upload status %d: %s", resp.StatusCode, data)
	}
	var uploaded struct {
		Artifact core.Artifact `json:"artifact"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&uploaded); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if uploaded.Artifact.ID == "" || uploaded.Artifact.Metadata["relative_path"] != "outputs/nested/result.txt" {
		t.Fatalf("unexpected artifact: %#v", uploaded.Artifact)
	}

	complete := leaseCompleteRequest{ExitCode: 0, Summary: "integration passed", OutputArtifactIDs: []string{uploaded.Artifact.ID}}
	doTestRequest(t, client, http.MethodPost, ts.URL+"/api/v1/leases/"+claim.LeaseID+"/complete", complete, leaseHeaders, http.StatusOK, &map[string]any{})

	var taskDetail struct {
		Task       core.Task        `json:"task"`
		Executions []core.Execution `json:"executions"`
	}
	doTestRequest(t, client, http.MethodGet, ts.URL+"/api/v1/tasks/"+submitted.Task.ID, nil, adminHeader(), http.StatusOK, &taskDetail)
	if taskDetail.Task.Status != core.TaskSucceeded || taskDetail.Task.Result == nil || len(taskDetail.Task.Result.OutputArtifactIDs) != 1 {
		t.Fatalf("task did not complete correctly: %#v", taskDetail.Task)
	}
	if len(taskDetail.Executions) != 1 || taskDetail.Executions[0].Status != core.ExecutionSucceeded {
		t.Fatalf("unexpected execution history: %#v", taskDetail.Executions)
	}

	var logs struct {
		ExecutionID string           `json:"execution_id"`
		Events      []store.LogEvent `json:"events"`
	}
	doTestRequest(t, client, http.MethodGet, ts.URL+"/api/v1/tasks/"+submitted.Task.ID+"/logs", nil, adminHeader(), http.StatusOK, &logs)
	if logs.ExecutionID != claim.ExecutionID || len(logs.Events) != 1 || logs.Events[0].Data != "integration output\n" {
		t.Fatalf("unexpected logs: %#v", logs)
	}

	downloadReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/artifacts/"+uploaded.Artifact.ID+"/content", nil)
	downloadReq.Header.Set("Authorization", "Bearer "+testAdminToken)
	download, err := client.Do(downloadReq)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(download.Body)
	download.Body.Close()
	if err != nil || download.StatusCode != http.StatusOK || string(data) != "artifact-ok" {
		t.Fatalf("download failed: status=%d data=%q err=%v", download.StatusCode, data, err)
	}
	if download.Header.Get("Cache-Control") != "private, no-store" {
		t.Fatalf("artifact response may be retained by browser caches: %q", download.Header.Get("Cache-Control"))
	}

	if err := stateStore.View(func(state *core.State) error {
		if state.Leases[claim.LeaseID].Status != core.LeaseCompleted {
			t.Fatalf("lease was not completed")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestMaintenanceCompactsLiveSandboxHistoryAndStaleArtifacts(t *testing.T) {
	ts, stateStore := newIntegrationServer(t)
	defer ts.Close()
	now := time.Now().UTC()
	sharedPath := filepath.Join("aa", strings.Repeat("a", 64))
	if err := os.MkdirAll(filepath.Join(stateStore.ArtifactsDir(), "aa"), 0o700); err != nil {
		t.Fatal(err)
	}
	contentPath := filepath.Join(stateStore.ArtifactsDir(), sharedPath)
	if err := os.WriteFile(contentPath, []byte("shared stale payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := stateStore.Update(func(state *core.State) error {
		state.Sandboxes["sbx_live"] = &core.Sandbox{ID: "sbx_live", NodeID: "node_live", Status: core.SandboxReady, TokenHash: core.HashSecret("sandbox-token-0123456789-0123456789")}
		state.SandboxOperations["sbxop_terminal"] = &core.SandboxOperation{
			ID: "sbxop_terminal", SandboxID: "sbx_live", NodeID: "node_live", Kind: "put", Status: core.SandboxOperationSucceeded,
			File: &core.SandboxFileSpec{Path: "/workspace/input", ArtifactID: "art_live"}, CreatedAt: now, UpdatedAt: now, FinishedAt: &now,
		}
		state.Artifacts["art_live"] = &core.Artifact{ID: "art_live", Name: "live", SHA256: strings.Repeat("a", 64), Size: 10, StoragePath: sharedPath, CreatedAt: now, Metadata: map[string]string{"sandbox_id": "sbx_live"}}
		state.Artifacts["art_stale"] = &core.Artifact{ID: "art_stale", Name: "stale", SHA256: strings.Repeat("a", 64), Size: 10, StoragePath: sharedPath, CreatedAt: now, Metadata: map[string]string{"sandbox_id": "sbx_missing"}}
		state.RegistryEvents = append(state.RegistryEvents,
			core.RegistryEvent{Revision: 1, Type: "device.observed"},
			core.RegistryEvent{Revision: 2, Type: "device.observed"},
			core.RegistryEvent{Revision: 3, Type: "device.description_updated"},
		)
		state.RegistryRevision = 3
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var preview maintenancePruneResult
	doTestRequest(t, ts.Client(), http.MethodPost, ts.URL+"/api/v1/maintenance/prune", maintenancePruneRequest{}, adminHeader(), http.StatusOK, &preview)
	if len(preview.SandboxOperationIDs) != 1 || len(preview.ArtifactIDs) != 2 || preview.DeviceObservationEvents != 2 {
		t.Fatalf("maintenance did not select live history and stale artifacts: %#v", preview)
	}
	var result maintenancePruneResult
	doTestRequest(t, ts.Client(), http.MethodPost, ts.URL+"/api/v1/maintenance/prune", maintenancePruneRequest{Execute: true}, adminHeader(), http.StatusOK, &result)
	if result.SandboxOperationsDeleted != 1 || result.ArtifactsDeleted != 2 || result.ArtifactContentFilesDeleted != 1 || result.RegistryObservationEventsDeleted != 2 {
		t.Fatalf("unexpected maintenance result: %#v", result)
	}
	if _, err := os.Stat(contentPath); !os.IsNotExist(err) {
		t.Fatalf("artifact content was not removed: %v", err)
	}
	_ = stateStore.View(func(state *core.State) error {
		if state.Sandboxes["sbx_live"] == nil || len(state.SandboxOperations) != 0 || len(state.Artifacts) != 0 {
			t.Fatalf("maintenance removed live sandbox or left history: %#v", state)
		}
		for _, event := range state.RegistryEvents {
			if event.Type == "device.observed" {
				t.Fatal("obsolete device observation event survived maintenance")
			}
		}
		return nil
	})
}

func TestAPIRequiresAdminAndHasNoBrowserControlSurface(t *testing.T) {
	ts, _ := newIntegrationServer(t)
	defer ts.Close()
	client := ts.Client()

	doTestRequest(t, client, http.MethodGet, ts.URL+"/api/v1/tasks", nil, nil, http.StatusUnauthorized, &APIError{})
	resp, err := client.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unexpected browser control surface: status=%d", resp.StatusCode)
	}
	if resp.Header.Get("Content-Security-Policy") == "" || resp.Header.Get("X-Frame-Options") != "DENY" {
		t.Fatalf("security headers missing: %#v", resp.Header)
	}
}

func TestClaimUsesServerLeaseCountAndClearsPreviousAttemptState(t *testing.T) {
	ts, stateStore := newIntegrationServer(t)
	defer ts.Close()
	client := ts.Client()

	var registration struct {
		Node struct {
			ID string `json:"id"`
		} `json:"node"`
		NodeToken string `json:"node_token"`
	}
	registerBody := map[string]any{
		"machine_id": "concurrency-machine", "name": "concurrency-worker",
		"capabilities": map[string]any{
			"hostname": "test", "os": "linux", "arch": "amd64", "cpu_threads": 4,
			"memory_total_mb": 8000, "disk_total_mb": 10000,
			"capabilities": []string{"executor.shell"}, "queues": []string{"default"}, "agent_version": config.Version,
		},
		"resources": map[string]any{
			"memory_available_mb": 7000, "disk_available_mb": 9000, "active_jobs": 99, "max_concurrency": 1,
		},
	}
	doTestRequest(t, client, http.MethodPost, ts.URL+"/api/v1/workers/register", registerBody, map[string]string{"X-AgentsPool-Join-Token": testJoinToken}, http.StatusCreated, &registration)
	nodeHeaders := map[string]string{"Authorization": "Bearer " + registration.NodeToken}

	submit := func(name string) string {
		t.Helper()
		var response taskSubmitResponse
		doTestRequest(t, client, http.MethodPost, ts.URL+"/api/v1/tasks", core.TaskSpec{
			Name: name, Executor: core.ExecutorSpec{Kind: core.ExecutorShell, Script: "true"},
		}, adminHeader(), http.StatusCreated, &response)
		return response.Task.ID
	}
	firstTaskID := submit("first concurrency task")
	secondTaskID := submit("second concurrency task")

	oldFinished := time.Now().UTC().Add(-time.Minute)
	if err := stateStore.Update(func(state *core.State) error {
		task := state.Tasks[firstTaskID]
		task.LastError = "previous attempt failed"
		task.Result = &core.TaskResult{ExitCode: 17, Summary: "stale"}
		task.FinishedAt = &oldFinished
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	claimBody := ClaimRequest{Resources: core.NodeResources{
		MemoryAvailableMB: 7000, DiskAvailableMB: 9000, ActiveJobs: 0, MaxConcurrency: 1,
	}}
	var firstClaim ClaimResponse
	doTestRequest(t, client, http.MethodPost, ts.URL+"/api/v1/workers/"+registration.Node.ID+"/claim", claimBody, nodeHeaders, http.StatusOK, &firstClaim)
	if !firstClaim.Claimed || firstClaim.Task == nil || firstClaim.Task.ID != firstTaskID {
		t.Fatalf("expected first task claim, got %#v", firstClaim)
	}
	if firstClaim.Task.LastError != "" || firstClaim.Task.Result != nil || firstClaim.Task.FinishedAt != nil {
		t.Fatalf("claim retained stale retry state: %#v", firstClaim.Task)
	}

	// Simulate a duplicate/stale worker process that falsely reports zero active jobs.
	var blockedClaim ClaimResponse
	doTestRequest(t, client, http.MethodPost, ts.URL+"/api/v1/workers/"+registration.Node.ID+"/claim", claimBody, nodeHeaders, http.StatusOK, &blockedClaim)
	if blockedClaim.Claimed {
		t.Fatalf("server exceeded max concurrency using worker-reported active_jobs: %#v", blockedClaim)
	}

	leaseHeaders := map[string]string{
		"Authorization":            "Bearer " + registration.NodeToken,
		"X-AgentsPool-Lease-Token": firstClaim.LeaseToken,
	}
	doTestRequest(t, client, http.MethodPost, ts.URL+"/api/v1/leases/"+firstClaim.LeaseID+"/start", struct{}{}, leaseHeaders, http.StatusOK, &map[string]any{})
	doTestRequest(t, client, http.MethodPost, ts.URL+"/api/v1/leases/"+firstClaim.LeaseID+"/complete", leaseCompleteRequest{ExitCode: 0}, leaseHeaders, http.StatusOK, &map[string]any{})

	var secondClaim ClaimResponse
	doTestRequest(t, client, http.MethodPost, ts.URL+"/api/v1/workers/"+registration.Node.ID+"/claim", claimBody, nodeHeaders, http.StatusOK, &secondClaim)
	if !secondClaim.Claimed || secondClaim.Task == nil || secondClaim.Task.ID != secondTaskID {
		t.Fatalf("expected second task after lease completion, got %#v", secondClaim)
	}

	if err := stateStore.View(func(state *core.State) error {
		node := state.Nodes[registration.Node.ID]
		if node == nil || node.Resources.ActiveJobs != 1 {
			t.Fatalf("node active job count not synchronized: %#v", node)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAdministrativeDataDeletionAndArtifactGC(t *testing.T) {
	ts, stateStore := newIntegrationServer(t)
	defer ts.Close()
	client := ts.Client()

	now := time.Now().UTC()
	taskID, executionID, leaseID := "task_delete_me", "exec_delete_me", "lease_delete_me"
	artifactID, storagePath := "art_delete_me", filepath.Join("aa", "delete-me")
	artifactPath := filepath.Join(stateStore.ArtifactsDir(), storagePath)
	if err := os.MkdirAll(filepath.Dir(artifactPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(artifactPath, []byte("sensitive artifact"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := stateStore.Update(func(state *core.State) error {
		state.Tasks[taskID] = &core.Task{
			ID: taskID, Status: core.TaskSucceeded, CreatedAt: now, UpdatedAt: now, FinishedAt: &now,
			Spec:   core.TaskSpec{Name: "delete sensitive task", IdempotencyKey: "delete-key", Executor: core.ExecutorSpec{Kind: core.ExecutorShell, Script: "true"}},
			Result: &core.TaskResult{ExitCode: 0, OutputArtifactIDs: []string{artifactID}},
		}
		state.Executions[executionID] = &core.Execution{ID: executionID, TaskID: taskID, LeaseID: leaseID, Status: core.ExecutionSucceeded, CreatedAt: now, FinishedAt: &now, OutputArtifactIDs: []string{artifactID}}
		state.Leases[leaseID] = &core.Lease{ID: leaseID, TaskID: taskID, ExecutionID: executionID, Status: core.LeaseCompleted, CreatedAt: now}
		state.Artifacts[artifactID] = &core.Artifact{ID: artifactID, SHA256: strings.Repeat("a", 64), Name: "secret.txt", MediaType: "text/plain", Size: 18, StoragePath: storagePath, CreatedAt: now, ProducerTaskID: taskID, ProducerExecutionID: executionID}
		state.Idempotency["delete-key"] = taskID
		state.Events = append(state.Events, core.Event{ID: "evt_sensitive", Timestamp: now, Type: "task.created", ResourceType: "task", ResourceID: taskID, Message: "sensitive task name"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	logStore := store.NewLogStore(stateStore.LogsDir())
	if err := logStore.Append(executionID, []store.LogEvent{{Sequence: 1, Timestamp: now, Stream: "stdout", Data: "secret log\n"}}); err != nil {
		t.Fatal(err)
	}

	var taskDelete map[string]any
	doTestRequest(t, client, http.MethodDelete, ts.URL+"/api/v1/tasks/"+taskID, nil, adminHeader(), http.StatusOK, &taskDelete)
	if taskDelete["deleted"] != true || int(taskDelete["logs_deleted"].(float64)) != 1 {
		t.Fatalf("unexpected task deletion response: %#v", taskDelete)
	}
	if err := stateStore.View(func(state *core.State) error {
		if state.Tasks[taskID] != nil || state.Executions[executionID] != nil || state.Leases[leaseID] != nil || state.Idempotency["delete-key"] != "" {
			t.Fatal("task deletion left state references")
		}
		if artifact := state.Artifacts[artifactID]; artifact == nil || artifact.ProducerTaskID != "" || artifact.ProducerExecutionID != "" {
			t.Fatalf("task deletion left artifact producer references: %#v", artifact)
		}
		for _, event := range state.Events {
			if event.ID == "evt_sensitive" {
				t.Fatal("task deletion retained sensitive historical event")
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(stateStore.LogsDir(), executionID+".jsonl")); !os.IsNotExist(err) {
		t.Fatalf("execution log still exists after task deletion: %v", err)
	}

	// Add a live reference and prove safe deletion refuses it without force.
	if err := stateStore.Update(func(state *core.State) error {
		state.Tasks["task_reference"] = &core.Task{ID: "task_reference", Status: core.TaskQueued, CreatedAt: now, UpdatedAt: now, Spec: core.TaskSpec{Name: "reference", Executor: core.ExecutorSpec{Kind: core.ExecutorShell, Script: "true"}, Inputs: []core.ArtifactInput{{ArtifactID: artifactID, Path: "input.txt"}}}}
		state.Artifacts["art_shared_content"] = &core.Artifact{ID: "art_shared_content", SHA256: strings.Repeat("a", 64), Name: "shared.txt", MediaType: "text/plain", Size: 18, StoragePath: storagePath, CreatedAt: now}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	doTestRequest(t, client, http.MethodDelete, ts.URL+"/api/v1/artifacts/"+artifactID, nil, adminHeader(), http.StatusConflict, &APIError{})
	var artifactDelete map[string]any
	doTestRequest(t, client, http.MethodDelete, ts.URL+"/api/v1/artifacts/"+artifactID+"?force=true", nil, adminHeader(), http.StatusOK, &artifactDelete)
	if artifactDelete["deleted"] != true || artifactDelete["content_deleted"] != false {
		t.Fatalf("unexpected artifact deletion response: %#v", artifactDelete)
	}
	if _, err := os.Stat(artifactPath); err != nil {
		t.Fatalf("shared artifact content was removed too early: %v", err)
	}
	if err := stateStore.View(func(state *core.State) error {
		if state.Artifacts[artifactID] != nil || state.Artifacts["art_shared_content"] == nil || len(state.Tasks["task_reference"].Spec.Inputs) != 0 {
			t.Fatal("forced artifact deletion left metadata references")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var sharedDelete map[string]any
	doTestRequest(t, client, http.MethodDelete, ts.URL+"/api/v1/artifacts/art_shared_content", nil, adminHeader(), http.StatusOK, &sharedDelete)
	if sharedDelete["content_deleted"] != true {
		t.Fatalf("last shared Artifact did not remove content: %#v", sharedDelete)
	}
	if _, err := os.Stat(artifactPath); !os.IsNotExist(err) {
		t.Fatalf("artifact content still exists after last reference deletion: %v", err)
	}

	orphanPath := filepath.Join(stateStore.ArtifactsDir(), "orphan.bin")
	if err := os.WriteFile(orphanPath, []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	var dryRun map[string]any
	doTestRequest(t, client, http.MethodPost, ts.URL+"/api/v1/artifacts/gc", map[string]any{}, adminHeader(), http.StatusOK, &dryRun)
	if dryRun["dry_run"] != true || int(dryRun["orphan_files"].(float64)) != 1 {
		t.Fatalf("unexpected artifact GC dry run: %#v", dryRun)
	}
	if _, err := os.Stat(orphanPath); err != nil {
		t.Fatalf("dry-run GC removed content: %v", err)
	}
	var gc map[string]any
	doTestRequest(t, client, http.MethodPost, ts.URL+"/api/v1/artifacts/gc", map[string]any{"delete": true}, adminHeader(), http.StatusOK, &gc)
	if int(gc["deleted_files"].(float64)) != 1 {
		t.Fatalf("unexpected artifact GC response: %#v", gc)
	}
	if _, err := os.Stat(orphanPath); !os.IsNotExist(err) {
		t.Fatalf("artifact GC left orphan content: %v", err)
	}
}
