package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vimalinx/AgentsPool/internal/client"
	"github.com/vimalinx/AgentsPool/internal/config"
	"github.com/vimalinx/AgentsPool/internal/core"
	"github.com/vimalinx/AgentsPool/internal/sandboxd"
)

type credentials struct {
	ServerURL    string    `json:"server_url"`
	MachineID    string    `json:"machine_id"`
	NodeID       string    `json:"node_id"`
	NodeToken    string    `json:"node_token"`
	RegisteredAt time.Time `json:"registered_at"`
}

type registerRequest struct {
	MachineID    string                `json:"machine_id"`
	Name         string                `json:"name"`
	Capabilities core.NodeCapabilities `json:"capabilities"`
	Resources    core.NodeResources    `json:"resources"`
}

type registerResponse struct {
	Node struct {
		ID string `json:"id"`
	} `json:"node"`
	NodeToken string `json:"node_token"`
}

type heartbeatRequest struct {
	Capabilities *core.NodeCapabilities `json:"capabilities,omitempty"`
	Resources    core.NodeResources     `json:"resources"`
}

type heartbeatResponse struct {
	ServerTime                   time.Time `json:"server_time"`
	NodeStatus                   string    `json:"node_status"`
	CancelLeaseIDs               []string  `json:"cancel_lease_ids"`
	CancelSandboxOperationIDs    []string  `json:"cancel_sandbox_operation_ids"`
	ReconcileSandboxOperationIDs []string  `json:"reconcile_sandbox_operation_ids"`
}

type claimRequest struct {
	Resources         core.NodeResources `json:"resources"`
	CachedArtifactIDs []string           `json:"cached_artifact_ids,omitempty"`
}

type claimResponse struct {
	Claimed         bool       `json:"claimed"`
	Task            *core.Task `json:"task,omitempty"`
	LeaseID         string     `json:"lease_id,omitempty"`
	LeaseToken      string     `json:"lease_token,omitempty"`
	ExecutionID     string     `json:"execution_id,omitempty"`
	LeaseExpiresAt  *time.Time `json:"lease_expires_at,omitempty"`
	LeaseTTLSeconds int        `json:"lease_ttl_seconds,omitempty"`
}

type jobHandle struct {
	cancel         context.CancelFunc
	adminCancelled atomic.Bool
	leaseLost      atomic.Bool
}

type sandboxOperationHandle struct {
	cancel         context.CancelFunc
	adminCancelled atomic.Bool
}

type sandboxHelper interface {
	ping(context.Context) error
	apply(context.Context, sandboxd.OperationRequest) (sandboxd.OperationResult, error)
	cancel(context.Context, string) error
	status(context.Context, string) (sandboxd.OperationStatus, error)
}

type Worker struct {
	cfg          config.Worker
	logger       *slog.Logger
	detector     *detector
	machineID    string
	capabilities core.NodeCapabilities
	credentials  credentials
	api          *client.Client
	cache        *artifactCache
	secrets      map[string]string
	sandbox      sandboxHelper

	activeMu sync.Mutex
	active   map[string]*jobHandle
	jobsWG   sync.WaitGroup

	sandboxActiveMu sync.Mutex
	sandboxActive   map[string]*sandboxOperationHandle
	sandboxWG       sync.WaitGroup

	serverDraining atomic.Bool
	fatalOnce      sync.Once
	fatalCh        chan error
	jobParent      context.Context
}

func withOptionalTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout > 0 {
		return context.WithTimeout(parent, timeout)
	}
	return context.WithCancel(parent)
}

func New(cfg config.Worker, logger *slog.Logger) (*Worker, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Worker{
		cfg: cfg, logger: logger, detector: newDetector(cfg),
		active: map[string]*jobHandle{}, sandboxActive: map[string]*sandboxOperationHandle{},
		fatalCh: make(chan error, 1),
	}, nil
}

func (w *Worker) Run(ctx context.Context) error {
	if err := os.MkdirAll(w.cfg.DataDir, 0o700); err != nil {
		return fmt.Errorf("create worker data directory: %w", err)
	}
	if err := os.Chmod(w.cfg.DataDir, 0o700); err != nil {
		return fmt.Errorf("protect worker data directory: %w", err)
	}
	machineID, err := resolveMachineID(w.cfg.DataDir, w.cfg.MachineID)
	if err != nil {
		return fmt.Errorf("resolve machine id: %w", err)
	}
	w.machineID = machineID
	w.capabilities = w.detector.Static(ctx)
	if w.cfg.SandboxSocket != "" {
		helper := newSandboxHelperClient(w.cfg.SandboxSocket)
		pingCtx, pingCancel := context.WithTimeout(ctx, 2*time.Second)
		pingErr := helper.ping(pingCtx)
		pingCancel()
		if pingErr == nil {
			w.sandbox = helper
			w.capabilities.Capabilities = appendUniqueStrings(w.capabilities.Capabilities, "sandbox.systemd-bwrap", "sandbox.persistent")
		} else {
			w.logger.Warn("sandbox helper unavailable; sandbox operations disabled", "socket", w.cfg.SandboxSocket, "error", pingErr)
		}
	}
	w.secrets, err = loadSecrets(w.cfg.SecretsFile)
	if err != nil {
		return fmt.Errorf("load worker secrets: %w", err)
	}
	if err := w.ensureCredentials(ctx); err != nil {
		return err
	}
	w.cache, err = newArtifactCache(filepath.Join(w.cfg.DataDir, "cache"), w.cfg.ArtifactCacheMaxBytes, w.api)
	if err != nil {
		return fmt.Errorf("initialize artifact cache: %w", err)
	}

	serviceCtx, serviceCancel := context.WithCancel(context.Background())
	claimCtx, claimCancel := context.WithCancel(serviceCtx)
	w.jobParent = serviceCtx
	defer serviceCancel()
	go w.heartbeatLoop(serviceCtx)
	go w.claimLoop(claimCtx)
	if w.sandbox != nil {
		go w.sandboxLoop(claimCtx)
	}
	w.logger.Info("AgentsPool worker online", "node_id", w.credentials.NodeID, "name", w.cfg.Name, "server", w.cfg.ServerURL, "max_concurrency", w.cfg.MaxConcurrency, "capabilities", w.capabilities.Capabilities)

	var runErr error
	select {
	case <-ctx.Done():
		w.logger.Info("worker shutdown requested; entering local drain")
	case runErr = <-w.fatalCh:
		w.logger.Error("worker stopping after fatal control-plane response", "error", runErr)
	}
	claimCancel()

	done := make(chan struct{})
	go func() {
		w.jobsWG.Wait()
		w.sandboxWG.Wait()
		close(done)
	}()
	if w.cfg.GracefulShutdownTimeout <= 0 {
		<-done
	} else {
		select {
		case <-done:
		case <-time.After(w.cfg.GracefulShutdownTimeout):
			w.logger.Warn("graceful timeout elapsed; abandoning active leases so the control plane can apply replay policy", "active_jobs", w.activeCount())
			w.abandonAllLeases()
			serviceCancel()
			<-done
		}
	}
	serviceCancel()
	return runErr
}

func appendUniqueStrings(values []string, additions ...string) []string {
	seen := make(map[string]bool, len(values)+len(additions))
	for _, value := range values {
		seen[value] = true
	}
	for _, value := range additions {
		if !seen[value] {
			values = append(values, value)
			seen[value] = true
		}
	}
	return values
}

func (w *Worker) ensureCredentials(ctx context.Context) error {
	path := filepath.Join(w.cfg.DataDir, "credentials.json")
	if err := os.Chmod(path, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("protect worker credentials: %w", err)
	}
	if data, err := os.ReadFile(path); err == nil {
		var stored credentials
		if json.Unmarshal(data, &stored) == nil && stored.MachineID == w.machineID && stored.NodeID != "" && stored.NodeToken != "" && (stored.ServerURL == w.cfg.ServerURL || isHTTPSUpgrade(stored.ServerURL, w.cfg.ServerURL)) {
			if stored.ServerURL != w.cfg.ServerURL {
				stored.ServerURL = w.cfg.ServerURL
				if err := writeCredentials(path, stored); err != nil {
					return err
				}
				if w.logger != nil {
					w.logger.Info("upgraded stored control-plane URL to HTTPS", "server", w.cfg.ServerURL)
				}
			}
			w.credentials = stored
			apiClient, clientErr := client.New(w.cfg.ServerURL, stored.NodeToken, w.cfg.RequestTimeout, w.cfg.InsecureSkipTLSVerify)
			if clientErr != nil {
				return clientErr
			}
			w.api = apiClient
			return nil
		}
	}
	if w.cfg.JoinToken == "" {
		return errors.New("worker has no valid credentials and AGENTSPOOL_JOIN_TOKEN/--join-token is not configured")
	}
	bootstrap, err := client.New(w.cfg.ServerURL, "", w.cfg.RequestTimeout, w.cfg.InsecureSkipTLSVerify)
	if err != nil {
		return err
	}
	request := registerRequest{MachineID: w.machineID, Name: w.cfg.Name, Capabilities: w.capabilities, Resources: w.detector.Dynamic(0)}
	var response registerResponse
	if err := bootstrap.DoJSON(ctx, http.MethodPost, "/api/v1/workers/register", nil, request, &response, map[string]string{"X-AgentsPool-Join-Token": w.cfg.JoinToken}); err != nil {
		return fmt.Errorf("register worker: %w", err)
	}
	if response.Node.ID == "" || response.NodeToken == "" {
		return errors.New("control plane returned incomplete worker credentials")
	}
	w.credentials = credentials{ServerURL: w.cfg.ServerURL, MachineID: w.machineID, NodeID: response.Node.ID, NodeToken: response.NodeToken, RegisteredAt: time.Now().UTC()}
	if err := writeCredentials(path, w.credentials); err != nil {
		return err
	}
	w.api, err = client.New(w.cfg.ServerURL, w.credentials.NodeToken, w.cfg.RequestTimeout, w.cfg.InsecureSkipTLSVerify)
	return err
}

func isHTTPSUpgrade(storedServer, configuredServer string) bool {
	stored, err := url.Parse(storedServer)
	if err != nil {
		return false
	}
	configured, err := url.Parse(configuredServer)
	if err != nil {
		return false
	}
	return stored.Scheme == "http" && configured.Scheme == "https" &&
		stored.Host == configured.Host && stored.EscapedPath() == configured.EscapedPath() &&
		stored.RawQuery == configured.RawQuery && stored.Fragment == configured.Fragment &&
		stored.User == nil && configured.User == nil
}

func writeCredentials(path string, value credentials) error {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode worker credentials: %w", err)
	}
	dirPath := filepath.Dir(path)
	tmp, err := os.CreateTemp(dirPath, ".credentials-*.tmp")
	if err != nil {
		return fmt.Errorf("create worker credentials: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return fmt.Errorf("protect worker credentials: %w", err)
	}
	if _, err := tmp.Write(encoded); err != nil {
		cleanup()
		return fmt.Errorf("write worker credentials: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("sync worker credentials: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close worker credentials: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("install worker credentials: %w", err)
	}
	if dir, openErr := os.Open(dirPath); openErr == nil {
		if syncErr := dir.Sync(); syncErr != nil {
			_ = dir.Close()
			return fmt.Errorf("sync worker credential directory: %w", syncErr)
		}
		_ = dir.Close()
	}
	return nil
}

func (w *Worker) heartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(w.cfg.HeartbeatInterval)
	defer ticker.Stop()
	for {
		if err := w.sendHeartbeat(ctx); err != nil {
			if isAuthenticationError(err) {
				w.reportFatal(fmt.Errorf("worker credentials rejected: %w", err))
				return
			}
			w.logger.Warn("worker heartbeat failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (w *Worker) sendHeartbeat(ctx context.Context) error {
	request := heartbeatRequest{Capabilities: &w.capabilities, Resources: w.detector.Dynamic(w.activeCount())}
	var response heartbeatResponse
	heartbeatCtx, cancel := context.WithTimeout(ctx, w.cfg.RequestTimeout)
	defer cancel()
	if err := w.api.DoJSON(heartbeatCtx, http.MethodPost, "/api/v1/workers/"+w.credentials.NodeID+"/heartbeat", nil, request, &response, nil); err != nil {
		return err
	}
	w.serverDraining.Store(response.NodeStatus != core.NodeOnline)
	for _, leaseID := range response.CancelLeaseIDs {
		w.cancelLease(leaseID, true)
	}
	for _, operationID := range response.CancelSandboxOperationIDs {
		w.cancelSandboxOperation(operationID, true)
	}
	for _, operationID := range response.ReconcileSandboxOperationIDs {
		w.reconcileSandboxOperation(operationID)
	}
	return nil
}

func (w *Worker) reconcileSandboxOperation(operationID string) {
	if w.sandbox == nil {
		return
	}
	statusCtx, statusCancel := context.WithTimeout(context.Background(), w.cfg.RequestTimeout)
	status, err := w.sandbox.status(statusCtx, operationID)
	statusCancel()
	if err != nil {
		w.logger.Warn("inspect orphaned sandbox operation failed", "operation_id", operationID, "error", err)
		return
	}
	// An active local handler may be flushing durable output after the unit
	// exited. Let its authenticated completion settle the result after recovery.
	w.sandboxActiveMu.Lock()
	locallyOwned := w.sandboxActive[operationID] != nil
	w.sandboxActiveMu.Unlock()
	if locallyOwned && !status.Running {
		return
	}
	request := struct {
		Running     bool   `json:"running"`
		ActiveState string `json:"active_state,omitempty"`
		SubState    string `json:"sub_state,omitempty"`
	}{Running: status.Running, ActiveState: status.ActiveState, SubState: status.SubState}
	reportCtx, reportCancel := context.WithTimeout(context.Background(), w.cfg.RequestTimeout)
	defer reportCancel()
	path := "/api/v1/workers/" + w.credentials.NodeID + "/sandbox-operations/" + operationID + "/reconcile"
	if err := w.api.DoJSON(reportCtx, http.MethodPost, path, nil, request, nil, nil); err != nil {
		w.logger.Warn("report orphaned sandbox operation status failed", "operation_id", operationID, "error", err)
	}
}

func (w *Worker) cancelSandboxOperation(operationID string, admin bool) {
	w.sandboxActiveMu.Lock()
	handle := w.sandboxActive[operationID]
	w.sandboxActiveMu.Unlock()
	if handle != nil {
		if admin {
			handle.adminCancelled.Store(true)
		}
		// Stop the deterministic transient unit and wait for systemd to
		// acknowledge it before cancelling the long-running apply request.
		// Cancelling only the HTTP request lets the Worker report cancellation
		// before sandboxd's deferred cleanup finishes, which can leave the port
		// occupied during an immediate redeploy or helper restart.
		if w.sandbox != nil {
			cancelCtx, cancel := context.WithTimeout(context.Background(), w.cfg.RequestTimeout)
			err := w.sandbox.cancel(cancelCtx, operationID)
			cancel()
			if err != nil {
				w.logger.Warn("stop active sandbox operation unit failed", "operation_id", operationID, "error", err)
			}
		}
		handle.cancel()
		return
	}
	// A Worker may restart after systemd accepted a transient unit. The new
	// Worker no longer has the original request context, but it can still stop
	// the deterministic unit by Operation ID.
	if w.sandbox != nil {
		ctx, cancel := context.WithTimeout(context.Background(), w.cfg.RequestTimeout)
		if err := w.sandbox.cancel(ctx, operationID); err != nil {
			cancel()
			w.logger.Warn("cancel orphaned sandbox operation failed", "operation_id", operationID, "error", err)
			return
		}
		cancel()
		reportCtx, reportCancel := context.WithTimeout(context.Background(), w.cfg.RequestTimeout)
		defer reportCancel()
		if err := w.api.DoJSON(reportCtx, http.MethodPost, "/api/v1/sandbox-operations/"+operationID+"/cancelled-by-node", nil, struct{}{}, nil, nil); err != nil {
			w.logger.Warn("reconcile orphaned sandbox operation cancellation failed", "operation_id", operationID, "error", err)
		}
	}
}

func (w *Worker) claimLoop(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		if w.serverDraining.Load() || w.activeCount() >= w.cfg.MaxConcurrency {
			if !sleepContext(ctx, 500*time.Millisecond) {
				return
			}
			continue
		}
		response, err := w.claim(ctx)
		if err != nil {
			if isAuthenticationError(err) {
				w.reportFatal(fmt.Errorf("claim authentication failed: %w", err))
				return
			}
			w.logger.Warn("task claim failed", "error", err)
			if !sleepContext(ctx, w.cfg.PollInterval) {
				return
			}
			continue
		}
		if !response.Claimed || response.Task == nil {
			if !sleepContext(ctx, w.cfg.PollInterval) {
				return
			}
			continue
		}
		w.launchJob(response)
	}
}

func (w *Worker) claim(ctx context.Context) (claimResponse, error) {
	request := claimRequest{Resources: w.detector.Dynamic(w.activeCount()), CachedArtifactIDs: w.cache.IDs(10000)}
	var response claimResponse
	claimCtx, cancel := context.WithTimeout(ctx, w.cfg.RequestTimeout)
	defer cancel()
	err := w.api.DoJSON(claimCtx, http.MethodPost, "/api/v1/workers/"+w.credentials.NodeID+"/claim", nil, request, &response, nil)
	return response, err
}

func (w *Worker) launchJob(claim claimResponse) {
	jobCtx, cancel := context.WithCancel(w.jobParent)
	handle := &jobHandle{cancel: cancel}
	w.activeMu.Lock()
	w.active[claim.LeaseID] = handle
	w.activeMu.Unlock()
	w.jobsWG.Add(1)
	go func() {
		defer w.jobsWG.Done()
		defer cancel()
		defer func() {
			w.activeMu.Lock()
			delete(w.active, claim.LeaseID)
			w.activeMu.Unlock()
		}()
		w.runJob(jobCtx, handle, claim)
	}()
}

func (w *Worker) activeCount() int {
	w.activeMu.Lock()
	defer w.activeMu.Unlock()
	return len(w.active)
}

func (w *Worker) cancelLease(leaseID string, admin bool) {
	w.activeMu.Lock()
	handle := w.active[leaseID]
	w.activeMu.Unlock()
	if handle != nil {
		if admin {
			handle.adminCancelled.Store(true)
		}
		handle.cancel()
	}
}

func (w *Worker) abandonAllLeases() {
	w.activeMu.Lock()
	handles := make([]*jobHandle, 0, len(w.active))
	for _, handle := range w.active {
		handles = append(handles, handle)
	}
	w.activeMu.Unlock()
	for _, handle := range handles {
		handle.leaseLost.Store(true)
		handle.cancel()
	}
}

func (w *Worker) reportFatal(err error) {
	w.fatalOnce.Do(func() { w.fatalCh <- err })
}

func isAuthenticationError(err error) bool {
	var apiErr *client.APIError
	return errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden)
}

func sleepContext(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (w *Worker) runJob(ctx context.Context, handle *jobHandle, claim claimResponse) {
	task := claim.Task
	logger := w.logger.With("task_id", task.ID, "execution_id", claim.ExecutionID, "lease_id", claim.LeaseID)
	logger.Info("starting claimed task", "name", task.Spec.Name, "attempt", task.Attempts, "executor", task.Spec.Executor.Kind)

	jobRoot := filepath.Join(w.cfg.DataDir, "jobs", task.ID, claim.ExecutionID)
	workspace := filepath.Join(jobRoot, "workspace")
	_ = os.RemoveAll(jobRoot)
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		w.reportJobFailure(context.Background(), claim, -1, "create job workspace: "+err.Error(), nil)
		return
	}

	batcher := newLogBatcher(context.Background(), w.api, claim.LeaseID, claim.LeaseToken, w.cfg.MaxLogChunkBytes, logger)
	emit := batcher.Emit
	emit("system", fmt.Sprintf("AgentsPool execution %s on node %s\n", claim.ExecutionID, w.credentials.NodeID))

	renewCtx, renewCancel := context.WithCancel(context.Background())
	renewDone := make(chan struct{})
	go func() {
		defer close(renewDone)
		w.renewLeaseLoop(renewCtx, handle, claim)
	}()
	finishLeaseActivity := func() {
		batcher.Close()
		renewCancel()
		<-renewDone
	}

	failBeforeExecution := func(err error) {
		emit("system", "Preparation failed: "+err.Error()+"\n")
		finishLeaseActivity()
		if !handle.leaseLost.Load() {
			w.reportJobFailure(context.Background(), claim, -1, err.Error(), nil)
		}
		w.cleanupJob(jobRoot, false)
	}

	for _, input := range task.Spec.Inputs {
		if ctx.Err() != nil {
			finishLeaseActivity()
			w.reportCancellationIfNeeded(claim, handle)
			w.cleanupJob(jobRoot, false)
			return
		}
		destination, err := safeJoin(workspace, input.Path)
		if err != nil {
			failBeforeExecution(fmt.Errorf("invalid input path %q: %w", input.Path, err))
			return
		}
		emit("system", fmt.Sprintf("Materializing input %s -> %s\n", input.ArtifactID, input.Path))
		transferCtx, transferCancel := withOptionalTimeout(ctx, w.cfg.TransferTimeout)
		err = w.cache.Materialize(transferCtx, input.ArtifactID, destination, input.Executable)
		transferCancel()
		if err != nil {
			failBeforeExecution(err)
			return
		}
	}

	if ctx.Err() != nil {
		finishLeaseActivity()
		w.reportCancellationIfNeeded(claim, handle)
		w.cleanupJob(jobRoot, false)
		return
	}
	if err := w.startLease(ctx, claim); err != nil {
		if handle.adminCancelled.Load() || ctx.Err() != nil {
			finishLeaseActivity()
			w.reportCancellationIfNeeded(claim, handle)
			w.cleanupJob(jobRoot, false)
			return
		}
		handle.leaseLost.Store(true)
		emit("system", "Lease start rejected: "+err.Error()+"\n")
		finishLeaseActivity()
		w.cleanupJob(jobRoot, false)
		return
	}

	execCtx := ctx
	execCancel := func() {}
	if task.Spec.TimeoutSeconds > 0 {
		timeout := time.Duration(task.Spec.TimeoutSeconds) * time.Second
		execCtx, execCancel = context.WithTimeout(ctx, timeout)
	}
	result := executeTask(execCtx, w.cfg, claim.ExecutionID, task.Spec.Executor, workspace, w.secrets, emit, logger)
	timedOut := errors.Is(execCtx.Err(), context.DeadlineExceeded)
	execCancel()

	if handle.leaseLost.Load() {
		emit("system", "Lease was lost; local execution stopped and no terminal result will be sent.\n")
		finishLeaseActivity()
		w.cleanupJob(jobRoot, false)
		return
	}
	if handle.adminCancelled.Load() {
		emit("system", "Execution cancelled by the control plane.\n")
		finishLeaseActivity()
		w.reportCancellationIfNeeded(claim, handle)
		w.cleanupJob(jobRoot, false)
		return
	}
	if result.Cancelled && !timedOut {
		emit("system", "Worker stopped the local process and abandoned the lease; server replay policy will decide the task outcome.\n")
		finishLeaseActivity()
		w.cleanupJob(jobRoot, false)
		return
	}

	outputs, outputErr := collectOutputFiles(workspace, task.Spec.Outputs, w.cfg.MaxOutputBytes)
	artifactIDs := []string{}
	if outputErr == nil {
		for _, output := range outputs {
			emit("system", fmt.Sprintf("Uploading output %s (%d bytes)\n", output.RelativePath, output.Size))
			artifactID, err := w.uploadOutput(ctx, claim, output)
			if err != nil {
				outputErr = err
				break
			}
			artifactIDs = append(artifactIDs, artifactID)
		}
	}

	success := result.Err == nil && result.ExitCode == 0 && outputErr == nil && !timedOut
	if timedOut {
		result.Err = fmt.Errorf("execution exceeded timeout of %s", time.Duration(task.Spec.TimeoutSeconds)*time.Second)
	}
	if outputErr != nil {
		result.Err = fmt.Errorf("collect or upload outputs: %w", outputErr)
		if result.ExitCode == 0 {
			result.ExitCode = -1
		}
	}
	if result.Err != nil {
		emit("system", "Execution failed: "+result.Err.Error()+"\n")
	}
	finishLeaseActivity()

	if success {
		if err := w.reportJobSuccess(context.Background(), claim, artifactIDs); err != nil {
			logger.Error("control plane rejected successful completion", "error", err)
			success = false
		}
	} else {
		reason := "execution failed"
		if result.Err != nil {
			reason = result.Err.Error()
		}
		if err := w.reportJobFailure(context.Background(), claim, result.ExitCode, reason, artifactIDs); err != nil {
			logger.Error("control plane rejected failure report", "error", err)
		}
	}
	w.cleanupJob(jobRoot, success)
	logger.Info("task execution finished", "success", success, "exit_code", result.ExitCode, "outputs", len(artifactIDs))
}

func (w *Worker) renewLeaseLoop(ctx context.Context, handle *jobHandle, claim claimResponse) {
	interval := time.Duration(claim.LeaseTTLSeconds) * time.Second / 3
	if interval < 3*time.Second {
		interval = 3 * time.Second
	}
	deadline := time.Now().Add(time.Duration(claim.LeaseTTLSeconds) * time.Second)
	if claim.LeaseExpiresAt != nil {
		deadline = *claim.LeaseExpiresAt
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			var response struct {
				ExpiresAt       time.Time `json:"expires_at"`
				CancelRequested bool      `json:"cancel_requested"`
			}
			renewCtx, cancel := context.WithTimeout(context.Background(), w.cfg.RequestTimeout)
			err := w.api.DoJSON(renewCtx, http.MethodPost, "/api/v1/leases/"+claim.LeaseID+"/renew", nil, struct{}{}, &response, map[string]string{"X-AgentsPool-Lease-Token": claim.LeaseToken})
			cancel()
			if err == nil {
				deadline = response.ExpiresAt
				if response.CancelRequested {
					handle.adminCancelled.Store(true)
					handle.cancel()
					return
				}
				continue
			}
			var apiErr *client.APIError
			if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusConflict || apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden) {
				handle.leaseLost.Store(true)
				handle.cancel()
				return
			}
			w.logger.Warn("lease renewal failed", "lease_id", claim.LeaseID, "error", err)
			if time.Now().After(deadline.Add(-time.Second)) {
				handle.leaseLost.Store(true)
				handle.cancel()
				return
			}
		}
	}
}

func (w *Worker) startLease(ctx context.Context, claim claimResponse) error {
	startCtx, cancel := context.WithTimeout(ctx, w.cfg.RequestTimeout)
	defer cancel()
	return w.api.DoJSON(startCtx, http.MethodPost, "/api/v1/leases/"+claim.LeaseID+"/start", nil, struct{}{}, nil, map[string]string{"X-AgentsPool-Lease-Token": claim.LeaseToken})
}

func (w *Worker) uploadOutput(ctx context.Context, claim claimResponse, output outputFile) (string, error) {
	query := url.Values{}
	query.Set("name", filepath.Base(output.RelativePath))
	query.Set("media_type", output.MediaType)
	query.Set("relative_path", output.RelativePath)
	var response struct {
		Artifact core.Artifact `json:"artifact"`
	}
	uploadCtx, cancel := withOptionalTimeout(ctx, w.cfg.TransferTimeout)
	defer cancel()
	if err := w.api.UploadFile(uploadCtx, "/api/v1/leases/"+claim.LeaseID+"/artifacts", query, output.AbsolutePath, map[string]string{"X-AgentsPool-Lease-Token": claim.LeaseToken, "Content-Type": output.MediaType}, &response); err != nil {
		return "", err
	}
	return response.Artifact.ID, nil
}

func (w *Worker) reportJobSuccess(ctx context.Context, claim claimResponse, artifactIDs []string) error {
	request := struct {
		ExitCode          int      `json:"exit_code"`
		Summary           string   `json:"summary,omitempty"`
		OutputArtifactIDs []string `json:"output_artifact_ids,omitempty"`
	}{ExitCode: 0, Summary: "completed by AgentsPool worker", OutputArtifactIDs: artifactIDs}
	reportCtx, cancel := context.WithTimeout(ctx, w.cfg.RequestTimeout)
	defer cancel()
	return w.api.DoJSON(reportCtx, http.MethodPost, "/api/v1/leases/"+claim.LeaseID+"/complete", nil, request, nil, map[string]string{"X-AgentsPool-Lease-Token": claim.LeaseToken})
}

func (w *Worker) reportJobFailure(ctx context.Context, claim claimResponse, exitCode int, reason string, artifactIDs []string) error {
	request := struct {
		ExitCode          int      `json:"exit_code"`
		Error             string   `json:"error"`
		OutputArtifactIDs []string `json:"output_artifact_ids,omitempty"`
	}{ExitCode: exitCode, Error: reason, OutputArtifactIDs: artifactIDs}
	reportCtx, cancel := context.WithTimeout(ctx, w.cfg.RequestTimeout)
	defer cancel()
	return w.api.DoJSON(reportCtx, http.MethodPost, "/api/v1/leases/"+claim.LeaseID+"/fail", nil, request, nil, map[string]string{"X-AgentsPool-Lease-Token": claim.LeaseToken})
}

func (w *Worker) reportCancellationIfNeeded(claim claimResponse, handle *jobHandle) {
	if handle.leaseLost.Load() || !handle.adminCancelled.Load() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), w.cfg.RequestTimeout)
	defer cancel()
	if err := w.api.DoJSON(ctx, http.MethodPost, "/api/v1/leases/"+claim.LeaseID+"/cancelled", nil, struct{}{}, nil, map[string]string{"X-AgentsPool-Lease-Token": claim.LeaseToken}); err != nil {
		w.logger.Warn("failed to acknowledge task cancellation", "lease_id", claim.LeaseID, "error", err)
	}
}

func (w *Worker) cleanupJob(path string, success bool) {
	keep := w.cfg.KeepJobs || (!success && w.cfg.KeepFailedJobs)
	if !keep {
		_ = os.RemoveAll(path)
	}
}
