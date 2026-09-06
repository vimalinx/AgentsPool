package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/vimalinx/AgentsPool/internal/client"
	"github.com/vimalinx/AgentsPool/internal/core"
	"github.com/vimalinx/AgentsPool/internal/sandboxd"
)

type sandboxHelperClient struct {
	http *http.Client
}

func newSandboxHelperClient(socketPath string) *sandboxHelperClient {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		},
	}
	return &sandboxHelperClient{http: &http.Client{Transport: transport}}
}

func (c *sandboxHelperClient) ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://sandboxd/v1/ping", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("sandbox helper ping returned %s", resp.Status)
	}
	return nil
}

func (c *sandboxHelperClient) apply(ctx context.Context, request sandboxd.OperationRequest) (sandboxd.OperationResult, error) {
	var result sandboxd.OperationResult
	data, err := json.Marshal(request)
	if err != nil {
		return result, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://sandboxd/v1/operations", bytes.NewReader(data))
	if err != nil {
		return result, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return result, err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return result, err
	}
	if resp.StatusCode != http.StatusOK {
		return result, fmt.Errorf("sandbox helper returned %s: %s", resp.Status, result.Error)
	}
	return result, nil
}

func (c *sandboxHelperClient) cancel(ctx context.Context, operationID string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://sandboxd/v1/operations/"+url.PathEscape(operationID)+"/cancel", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("sandbox helper cancel returned %s", resp.Status)
	}
	return nil
}

func (c *sandboxHelperClient) status(ctx context.Context, operationID string) (sandboxd.OperationStatus, error) {
	var result sandboxd.OperationStatus
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://sandboxd/v1/operations/"+url.PathEscape(operationID), nil)
	if err != nil {
		return result, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return result, err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return result, err
	}
	if resp.StatusCode != http.StatusOK {
		return result, fmt.Errorf("sandbox helper status returned %s", resp.Status)
	}
	return result, nil
}

type sandboxOperationClaim struct {
	Claimed              bool                   `json:"claimed"`
	Operation            *core.SandboxOperation `json:"operation,omitempty"`
	OperationToken       string                 `json:"operation_token,omitempty"`
	Sandbox              *core.Sandbox          `json:"sandbox,omitempty"`
	Resources            *core.ResourceRequest  `json:"resources,omitempty"`
	ResourceLeaseExpires *time.Time             `json:"resource_lease_expires_at,omitempty"`
}

func (w *Worker) sandboxLoop(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		if w.serverDraining.Load() {
			// The control plane still permits stop/destroy claims during drain,
			// so keep polling; it rejects new compute operations authoritatively.
			if !sleepContext(ctx, w.cfg.PollInterval) {
				return
			}
		}
		var claim sandboxOperationClaim
		claimCtx, cancel := context.WithTimeout(ctx, w.cfg.RequestTimeout)
		err := w.api.DoJSON(claimCtx, http.MethodPost, "/api/v1/workers/"+w.credentials.NodeID+"/sandbox-operations/claim", nil, struct{}{}, &claim, nil)
		cancel()
		if err != nil {
			if isAuthenticationError(err) {
				w.reportFatal(fmt.Errorf("sandbox operation authentication failed: %w", err))
				return
			}
			w.logger.Warn("sandbox operation claim failed", "error", err)
			if !sleepContext(ctx, w.cfg.PollInterval) {
				return
			}
			continue
		}
		if !claim.Claimed || claim.Operation == nil || claim.Sandbox == nil || claim.Resources == nil {
			if !sleepContext(ctx, w.cfg.PollInterval) {
				return
			}
			continue
		}
		w.sandboxWG.Add(1)
		go func() {
			defer w.sandboxWG.Done()
			// claimCtx stops new claims during drain. The Operation itself belongs
			// to serviceCtx so a Worker restart waits instead of inventing a
			// shutdown deadline or killing a long-running service.
			w.runSandboxOperation(w.jobParent, claim)
		}()
	}
}

func (w *Worker) runSandboxOperation(ctx context.Context, claim sandboxOperationClaim) {
	operation := claim.Operation
	logger := w.logger.With("sandbox_id", claim.Sandbox.ID, "operation_id", operation.ID, "kind", operation.Kind)
	operationCtx, operationCancel := context.WithCancel(ctx)
	cancel := operationCancel
	handle := &sandboxOperationHandle{cancel: operationCancel}
	w.sandboxActiveMu.Lock()
	w.sandboxActive[operation.ID] = handle
	w.sandboxActiveMu.Unlock()
	defer func() {
		w.sandboxActiveMu.Lock()
		delete(w.sandboxActive, operation.ID)
		w.sandboxActiveMu.Unlock()
	}()
	if operation.Exec != nil && operation.Exec.TimeoutSeconds > 0 {
		var deadlineCancel context.CancelFunc
		operationCtx, deadlineCancel = context.WithTimeout(operationCtx, core.SecondsDuration(operation.Exec.TimeoutSeconds))
		previousCancel := cancel
		cancel = func() { deadlineCancel(); previousCancel() }
	}
	defer cancel()
	startErr := w.retrySandboxControlRequest(operationCtx, operation.ID, "start", func(requestCtx context.Context) error {
		return w.api.DoJSON(requestCtx, http.MethodPost, "/api/v1/sandbox-operations/"+operation.ID+"/start", nil, struct{}{}, nil, map[string]string{"X-AgentsPool-Sandbox-Operation-Token": claim.OperationToken})
	})
	if startErr != nil {
		logger.Error("sandbox operation start was not accepted", "error", startErr)
		if operationCtx.Err() != nil {
			w.reportSandboxOperationFailure(claim, "sandbox operation was cancelled before start was accepted")
		}
		return
	}
	policyCtx, policyCancel := context.WithCancel(operationCtx)
	defer policyCancel()
	policyDone := make(chan struct{})
	if operation.Kind != "stop" && operation.Kind != "destroy" {
		go func() { defer close(policyDone); w.monitorSandboxLease(policyCtx, operationCancel, claim) }()
	} else {
		close(policyDone)
	}
	defer func() { policyCancel(); <-policyDone }()
	requestToHelper := sandboxd.OperationRequest{Operation: *operation, Sandbox: *claim.Sandbox, Resources: *claim.Resources}
	transferDir := ""
	if operation.Kind == "put" || operation.Kind == "get" {
		if operation.File == nil {
			w.reportSandboxOperationFailure(claim, "file operation is missing its file spec")
			return
		}
		transferDir = filepath.Join(w.cfg.DataDir, "transfers", operation.ID)
		if err := os.MkdirAll(transferDir, 0o700); err != nil {
			w.reportSandboxOperationFailure(claim, "create sandbox transfer directory: "+err.Error())
			return
		}
		defer os.RemoveAll(transferDir)
		requestToHelper.TransferPath = filepath.Join(transferDir, "payload")
		if operation.Kind == "put" {
			transferCtx, transferCancel := withOptionalTimeout(operationCtx, w.cfg.TransferTimeout)
			err := w.cache.Materialize(transferCtx, operation.File.ArtifactID, requestToHelper.TransferPath, operation.File.Executable)
			transferCancel()
			if err != nil {
				w.reportSandboxOperationFailure(claim, "materialize sandbox input artifact: "+err.Error())
				return
			}
		}
	}
	executionDone := make(chan struct{})
	logsDone := make(chan error, 1)
	if operation.Kind == "exec" {
		go func() { logsDone <- w.streamSandboxLogs(w.jobParent, claim, executionDone) }()
	} else {
		logsDone <- nil
	}
	result, err := w.sandbox.apply(operationCtx, requestToHelper)
	if operation.Kind == "exec" && err != nil {
		// A cancelled HTTP call does not acknowledge remote process exit.
		// Keep the reservation and wait for an idempotent stop acknowledgement.
		if stopErr := w.retrySandboxControlRequest(w.jobParent, operation.ID, "confirm helper termination", func(requestCtx context.Context) error {
			return w.sandbox.cancel(requestCtx, operation.ID)
		}); stopErr != nil {
			close(executionDone)
			logger.Error("sandbox termination could not be confirmed", "error", stopErr)
			return
		}
	}
	close(executionDone)
	policyCancel()
	<-policyDone
	if logErr := <-logsDone; logErr != nil {
		result.Success = false
		result.Error = "sandbox log delivery failed: " + logErr.Error()
	}
	if err != nil && result.Error == "" {
		result.Error = err.Error()
		result.Success = false
	}
	if result.Success && operation.Kind == "get" {
		query := url.Values{"media_type": {"application/octet-stream"}}
		var uploaded struct {
			Artifact core.Artifact `json:"artifact"`
		}
		uploadCtx, uploadCancel := withOptionalTimeout(context.Background(), w.cfg.TransferTimeout)
		uploadErr := w.api.UploadFile(uploadCtx, "/api/v1/sandbox-operations/"+operation.ID+"/artifact", query, requestToHelper.TransferPath, map[string]string{"X-AgentsPool-Sandbox-Operation-Token": claim.OperationToken, "Content-Type": "application/octet-stream"}, &uploaded)
		uploadCancel()
		if uploadErr != nil {
			result.Success = false
			result.Error = "upload sandbox export: " + uploadErr.Error()
		}
	}
	request := struct {
		Success         bool   `json:"success"`
		Cancelled       bool   `json:"cancelled,omitempty"`
		ExitCode        *int   `json:"exit_code,omitempty"`
		Stdout          string `json:"stdout,omitempty"`
		Stderr          string `json:"stderr,omitempty"`
		Error           string `json:"error,omitempty"`
		OutputTruncated bool   `json:"output_truncated,omitempty"`
	}{Success: result.Success, Cancelled: handle.adminCancelled.Load(), ExitCode: result.ExitCode, Stdout: result.Stdout, Stderr: result.Stderr, Error: result.Error, OutputTruncated: result.OutputTruncated}
	if reportErr := w.retrySandboxControlRequest(w.jobParent, operation.ID, "complete", func(requestCtx context.Context) error {
		return w.api.DoJSON(requestCtx, http.MethodPost, "/api/v1/sandbox-operations/"+operation.ID+"/complete", nil, request, nil, map[string]string{"X-AgentsPool-Sandbox-Operation-Token": claim.OperationToken})
	}); reportErr != nil {
		logger.Error("sandbox operation result rejected", "error", reportErr)
		return
	}
	logger.Info("sandbox operation finished", "success", result.Success, "exit_code", result.ExitCode)
}

func (w *Worker) monitorSandboxLease(ctx context.Context, cancel context.CancelFunc, claim sandboxOperationClaim) {
	expires := claim.ResourceLeaseExpires
	interval := w.cfg.PollInterval
	if interval <= 0 || interval > time.Second {
		interval = time.Second
	}
	for {
		// A stalled refresh must not extend the last confirmed finite lease.
		requestDeadline := time.Now().Add(w.cfg.RequestTimeout)
		if expires != nil && expires.Before(requestDeadline) {
			requestDeadline = *expires
		}
		requestCtx, requestCancel := context.WithDeadline(ctx, requestDeadline)
		var policy core.SandboxLeasePolicy
		err := w.api.DoJSON(requestCtx, http.MethodGet,
			"/api/v1/workers/"+claim.Operation.NodeID+"/sandbox-operations/"+claim.Operation.ID+"/lease",
			nil, nil, &policy, map[string]string{"X-AgentsPool-Sandbox-Operation-Token": claim.OperationToken})
		requestCancel()
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			if !policy.Active {
				cancel()
				return
			}
			expires = policy.ExpiresAt
		} else {
			var apiErr *client.APIError
			if errors.As(err, &apiErr) && apiErr.StatusCode >= 400 && apiErr.StatusCode < 500 {
				cancel()
				return
			}
			// During a partition, a finite lease fails closed at the last
			// confirmed deadline. Unlimited leases have no invented deadline.
			if expires != nil && !expires.After(time.Now()) {
				cancel()
				return
			}
		}
		wait := interval
		if expires != nil {
			remaining := time.Until(*expires)
			if remaining <= 0 {
				cancel()
				return
			}
			// Refresh before expiry, leaving time to observe a recent renewal.
			if remaining/2 < wait {
				wait = remaining / 2
			}
			if wait < 10*time.Millisecond {
				wait = min(remaining, 10*time.Millisecond)
			}
		}
		if !sleepContext(ctx, wait) {
			return
		}
	}
}

func (w *Worker) reportSandboxOperationFailure(claim sandboxOperationClaim, message string) {
	request := struct {
		Success bool   `json:"success"`
		Error   string `json:"error"`
	}{Success: false, Error: message}
	if err := w.retrySandboxControlRequest(w.jobParent, claim.Operation.ID, "report preparation failure", func(requestCtx context.Context) error {
		return w.api.DoJSON(requestCtx, http.MethodPost, "/api/v1/sandbox-operations/"+claim.Operation.ID+"/complete", nil, request, nil, map[string]string{"X-AgentsPool-Sandbox-Operation-Token": claim.OperationToken})
	}); err != nil {
		w.logger.Error("sandbox operation preparation failure was rejected", "operation_id", claim.Operation.ID, "error", err)
	}
}

func (w *Worker) retrySandboxControlRequest(ctx context.Context, operationID, action string, request func(context.Context) error) error {
	for {
		requestCtx, cancel := context.WithTimeout(ctx, w.cfg.RequestTimeout)
		err := request(requestCtx)
		cancel()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var apiErr *client.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode < http.StatusInternalServerError {
			if isAuthenticationError(err) {
				w.reportFatal(fmt.Errorf("sandbox operation credentials rejected: %w", err))
			}
			return err
		}
		w.logger.Warn("sandbox control report will retry", "operation_id", operationID, "action", action, "error", err)
		if !sleepContext(ctx, w.cfg.PollInterval) {
			return ctx.Err()
		}
	}
}
