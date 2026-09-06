package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/vimalinx/AgentsPool/internal/client"
	"github.com/vimalinx/AgentsPool/internal/config"
)

func TestSandboxControlReportRetriesTransientFailureWithoutOverallDeadline(t *testing.T) {
	w := &Worker{
		cfg:     config.Worker{RequestTimeout: 20 * time.Millisecond, PollInterval: time.Millisecond},
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		fatalCh: make(chan error, 1),
	}
	attempts := 0
	err := w.retrySandboxControlRequest(context.Background(), "sbxop_test", "complete", func(context.Context) error {
		attempts++
		if attempts < 3 {
			return errors.New("control plane restarting")
		}
		return nil
	})
	if err != nil || attempts != 3 {
		t.Fatalf("retry result: attempts=%d err=%v", attempts, err)
	}
}

func TestSandboxControlReportDoesNotRetryTerminalConflict(t *testing.T) {
	w := &Worker{
		cfg:     config.Worker{RequestTimeout: 20 * time.Millisecond, PollInterval: time.Millisecond},
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		fatalCh: make(chan error, 1),
	}
	attempts := 0
	err := w.retrySandboxControlRequest(context.Background(), "sbxop_test", "complete", func(context.Context) error {
		attempts++
		return &client.APIError{StatusCode: http.StatusConflict, Code: "operation_terminal", Message: "already terminal"}
	})
	if err == nil || attempts != 1 {
		t.Fatalf("terminal conflict should stop immediately: attempts=%d err=%v", attempts, err)
	}
}
