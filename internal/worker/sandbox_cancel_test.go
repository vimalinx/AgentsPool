package worker

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/qiyuehuajin/agentspool/internal/config"
	"github.com/qiyuehuajin/agentspool/internal/sandboxd"
)

type recordingSandboxHelper struct {
	mu    sync.Mutex
	order []string
}

func (h *recordingSandboxHelper) ping(context.Context) error { return nil }

func (h *recordingSandboxHelper) apply(context.Context, sandboxd.OperationRequest) (sandboxd.OperationResult, error) {
	return sandboxd.OperationResult{}, nil
}

func (h *recordingSandboxHelper) cancel(context.Context, string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.order = append(h.order, "helper")
	return nil
}

func (h *recordingSandboxHelper) status(context.Context, string) (sandboxd.OperationStatus, error) {
	return sandboxd.OperationStatus{}, nil
}

func TestActiveSandboxCancellationStopsUnitBeforeCancellingRequest(t *testing.T) {
	helper := &recordingSandboxHelper{}
	handle := &sandboxOperationHandle{cancel: func() {
		helper.mu.Lock()
		defer helper.mu.Unlock()
		helper.order = append(helper.order, "request")
	}}
	worker := &Worker{
		cfg:           config.Worker{RequestTimeout: time.Second},
		logger:        slog.Default(),
		sandbox:       helper,
		sandboxActive: map[string]*sandboxOperationHandle{"sbxop_test": handle},
	}
	worker.cancelSandboxOperation("sbxop_test", true)
	if !handle.adminCancelled.Load() {
		t.Fatal("admin cancellation was not recorded")
	}
	helper.mu.Lock()
	defer helper.mu.Unlock()
	if len(helper.order) != 2 || helper.order[0] != "helper" || helper.order[1] != "request" {
		t.Fatalf("cancellation order = %v, want [helper request]", helper.order)
	}
}
