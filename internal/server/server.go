package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vimalinx/AgentsPool/internal/config"
	"github.com/vimalinx/AgentsPool/internal/logstream"
	"github.com/vimalinx/AgentsPool/internal/store"
)

type Metrics struct {
	RequestsTotal       atomic.Uint64
	ClaimsTotal         atomic.Uint64
	CompletionsTotal    atomic.Uint64
	FailuresTotal       atomic.Uint64
	LeaseExpirations    atomic.Uint64
	ArtifactsUploaded   atomic.Uint64
	BytesUploaded       atomic.Uint64
	AuthenticationFails atomic.Uint64
}

type Server struct {
	cfg             config.Server
	store           *store.Store
	logs            *store.LogStore
	operationLogs   *logstream.Store
	logger          *slog.Logger
	metrics         Metrics
	startedAt       time.Time
	handler         http.Handler
	registryMu      sync.Mutex
	registryChanged chan struct{}
	artifactMu      sync.Mutex
}

func New(cfg config.Server, stateStore *store.Store, logger *slog.Logger) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if stateStore == nil {
		return nil, errors.New("state store is required")
	}
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(os.Stdout, nil))
	}
	s := &Server{
		cfg:             cfg,
		store:           stateStore,
		logs:            store.NewLogStore(stateStore.LogsDir()),
		operationLogs:   &logstream.Store{Dir: filepath.Join(stateStore.LogsDir(), "sandbox-operations"), RetainBytes: cfg.SandboxLogRetainBytes},
		logger:          logger,
		startedAt:       time.Now().UTC(),
		registryChanged: make(chan struct{}),
	}
	if err := s.recoverState(); err != nil {
		return nil, fmt.Errorf("recover server state: %w", err)
	}
	s.handler = s.buildHandler()
	return s, nil
}

func (s *Server) Handler() http.Handler { return s.handler }

func (s *Server) Run(ctx context.Context) error {
	httpServer := &http.Server{
		Addr:              s.cfg.ListenAddress,
		Handler:           s.handler,
		ReadHeaderTimeout: 10 * time.Second,
		// Authenticated Artifact transfers can legitimately exceed a minute.
		// Body sizes are bounded per route; client contexts bound transfer time.
		ReadTimeout:  0,
		WriteTimeout: 0,
		IdleTimeout:  90 * time.Second,
	}

	go s.runReaper(ctx)
	errCh := make(chan error, 1)
	go func() {
		s.logger.Info("AgentsPool control plane listening", "address", s.cfg.ListenAddress, "data_dir", s.cfg.DataDir, "tls", s.cfg.TLSCertFile != "")
		if s.cfg.TLSCertFile != "" {
			errCh <- httpServer.ListenAndServeTLS(s.cfg.TLSCertFile, s.cfg.TLSKeyFile)
			return
		}
		errCh <- httpServer.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
		_ = s.store.Flush()
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func (s *Server) buildHandler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /readyz", s.handleReady)
	mux.HandleFunc("GET /metrics", s.handleMetrics)

	mux.HandleFunc("POST /api/v1/workers/register", s.handleWorkerRegister)
	mux.HandleFunc("POST /api/v1/workers/{nodeID}/heartbeat", s.handleWorkerHeartbeat)
	mux.HandleFunc("POST /api/v1/workers/{nodeID}/claim", s.handleWorkerClaim)

	mux.HandleFunc("POST /api/v1/leases/{leaseID}/start", s.handleLeaseStart)
	mux.HandleFunc("POST /api/v1/leases/{leaseID}/renew", s.handleLeaseRenew)
	mux.HandleFunc("POST /api/v1/leases/{leaseID}/logs", s.handleLeaseLogs)
	mux.HandleFunc("POST /api/v1/leases/{leaseID}/artifacts", s.handleLeaseArtifactUpload)
	mux.HandleFunc("POST /api/v1/leases/{leaseID}/complete", s.handleLeaseComplete)
	mux.HandleFunc("POST /api/v1/leases/{leaseID}/fail", s.handleLeaseFail)
	mux.HandleFunc("POST /api/v1/leases/{leaseID}/cancelled", s.handleLeaseCancelled)

	mux.HandleFunc("GET /api/v1/status", s.handleStatus)
	mux.HandleFunc("GET /api/v1/nodes", s.handleNodesList)
	mux.HandleFunc("GET /api/v1/nodes/{nodeID}", s.handleNodeGet)
	mux.HandleFunc("GET /api/v1/devices", s.handleDevicesList)
	mux.HandleFunc("GET /api/v1/devices/{nodeID}", s.handleDeviceGet)
	mux.HandleFunc("PUT /api/v1/devices/{nodeID}/descriptions", s.handleDeviceDescriptionUpsert)
	mux.HandleFunc("DELETE /api/v1/devices/{nodeID}/descriptions", s.handleDeviceDescriptionRemove)
	mux.HandleFunc("GET /api/v1/devices-watch", s.handleDevicesWatch)
	mux.HandleFunc("GET /api/v1/services", s.handleServicesList)
	mux.HandleFunc("POST /api/v1/resource-leases", s.handleResourceLeaseAcquire)
	mux.HandleFunc("GET /api/v1/resource-leases", s.handleResourceLeasesList)
	mux.HandleFunc("GET /api/v1/resource-leases/{leaseID}", s.handleResourceLeaseGet)
	mux.HandleFunc("POST /api/v1/resource-leases/{leaseID}/renew", s.handleResourceLeaseRenew)
	mux.HandleFunc("POST /api/v1/resource-leases/{leaseID}/resize", s.handleResourceLeaseResize)
	mux.HandleFunc("DELETE /api/v1/resource-leases/{leaseID}", s.handleResourceLeaseRelease)
	mux.HandleFunc("POST /api/v1/sandboxes", s.handleSandboxCreate)
	mux.HandleFunc("GET /api/v1/sandboxes", s.handleSandboxesList)
	mux.HandleFunc("GET /api/v1/sandboxes/{sandboxID}", s.handleSandboxGet)
	mux.HandleFunc("POST /api/v1/sandboxes/{sandboxID}/exec", s.handleSandboxExec)
	mux.HandleFunc("POST /api/v1/sandboxes/{sandboxID}/files", s.handleSandboxFilePush)
	mux.HandleFunc("POST /api/v1/sandboxes/{sandboxID}/files/export", s.handleSandboxFileExport)
	mux.HandleFunc("GET /api/v1/sandboxes/{sandboxID}/artifacts", s.handleSandboxArtifactsList)
	mux.HandleFunc("DELETE /api/v1/sandboxes/{sandboxID}/artifacts/{artifactID}", s.handleSandboxArtifactDelete)
	mux.HandleFunc("GET /api/v1/sandboxes/{sandboxID}/artifacts/{artifactID}/content", s.handleSandboxArtifactDownload)
	mux.HandleFunc("POST /api/v1/sandboxes/{sandboxID}/resize", s.handleSandboxResize)
	mux.HandleFunc("POST /api/v1/sandboxes/{sandboxID}/rebind", s.handleSandboxRebind)
	mux.HandleFunc("POST /api/v1/sandboxes/{sandboxID}/start", s.handleSandboxStart)
	mux.HandleFunc("POST /api/v1/sandboxes/{sandboxID}/stop", s.handleSandboxStop)
	mux.HandleFunc("DELETE /api/v1/sandboxes/{sandboxID}", s.handleSandboxDestroy)
	mux.HandleFunc("GET /api/v1/sandbox-operations/{operationID}", s.handleSandboxOperationGet)
	mux.HandleFunc("GET /api/v1/sandbox-operations/{operationID}/logs", s.handleSandboxOperationLogs)
	mux.HandleFunc("POST /api/v1/sandbox-operations/{operationID}/logs", s.handleSandboxOperationLogsUpload)
	mux.HandleFunc("POST /api/v1/sandbox-operations/{operationID}/cancel", s.handleSandboxOperationCancel)
	mux.HandleFunc("POST /api/v1/workers/{nodeID}/sandbox-operations/claim", s.handleSandboxOperationClaim)
	mux.HandleFunc("GET /api/v1/workers/{nodeID}/sandbox-operations/{operationID}/lease", s.handleSandboxOperationLease)
	mux.HandleFunc("POST /api/v1/workers/{nodeID}/sandbox-operations/{operationID}/reconcile", s.handleSandboxOperationReconcile)
	mux.HandleFunc("POST /api/v1/sandbox-operations/{operationID}/start", s.handleSandboxOperationStart)
	mux.HandleFunc("POST /api/v1/sandbox-operations/{operationID}/cancelled-by-node", s.handleSandboxOperationCancelledByNode)
	mux.HandleFunc("POST /api/v1/sandbox-operations/{operationID}/complete", s.handleSandboxOperationComplete)
	mux.HandleFunc("POST /api/v1/sandbox-operations/{operationID}/artifact", s.handleSandboxOperationArtifactUpload)
	mux.HandleFunc("POST /api/v1/nodes/{nodeID}/drain", s.handleNodeDrain)
	mux.HandleFunc("POST /api/v1/nodes/{nodeID}/enable", s.handleNodeEnable)
	mux.HandleFunc("POST /api/v1/nodes/{nodeID}/disable", s.handleNodeDisable)
	mux.HandleFunc("DELETE /api/v1/nodes/{nodeID}", s.handleNodeDelete)

	mux.HandleFunc("POST /api/v1/tasks", s.handleTaskSubmit)
	mux.HandleFunc("GET /api/v1/tasks", s.handleTasksList)
	mux.HandleFunc("GET /api/v1/tasks/{taskID}", s.handleTaskGet)
	mux.HandleFunc("DELETE /api/v1/tasks/{taskID}", s.handleTaskDelete)
	mux.HandleFunc("POST /api/v1/tasks/{taskID}/cancel", s.handleTaskCancel)
	mux.HandleFunc("POST /api/v1/tasks/{taskID}/retry", s.handleTaskRetry)
	mux.HandleFunc("GET /api/v1/tasks/{taskID}/logs", s.handleTaskLogs)
	mux.HandleFunc("GET /api/v1/executions/{executionID}/logs", s.handleExecutionLogs)

	mux.HandleFunc("POST /api/v1/artifacts", s.handleAdminArtifactUpload)
	mux.HandleFunc("GET /api/v1/artifacts", s.handleArtifactsList)
	mux.HandleFunc("POST /api/v1/artifacts/gc", s.handleArtifactsGC)
	mux.HandleFunc("GET /api/v1/artifacts/{artifactID}", s.handleArtifactGet)
	mux.HandleFunc("DELETE /api/v1/artifacts/{artifactID}", s.handleArtifactDelete)
	mux.HandleFunc("GET /api/v1/artifacts/{artifactID}/content", s.handleArtifactDownload)
	mux.HandleFunc("GET /api/v1/events", s.handleEventsList)
	mux.HandleFunc("POST /api/v1/maintenance/prune", s.handleMaintenancePrune)

	return s.recoverMiddleware(s.securityHeadersMiddleware(s.loggingMiddleware(mux)))
}

func (s *Server) registryWaitChannel() <-chan struct{} {
	s.registryMu.Lock()
	defer s.registryMu.Unlock()
	return s.registryChanged
}

func (s *Server) notifyRegistryChanged() {
	s.registryMu.Lock()
	close(s.registryChanged)
	s.registryChanged = make(chan struct{})
	s.registryMu.Unlock()
}

func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.metrics.RequestsTotal.Add(1)
		started := time.Now()
		requestID := strings.TrimSpace(r.Header.Get("X-Request-ID"))
		if requestID == "" {
			requestID = newIDOrFallback("req")
		}
		w.Header().Set("X-Request-ID", requestID)
		next.ServeHTTP(w, r)
		s.logger.Debug("http request", "method", r.Method, "path", r.URL.Path, "duration_ms", time.Since(started).Milliseconds(), "request_id", requestID)
	})
}

func (s *Server) securityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				s.logger.Error("panic in HTTP handler", "panic", recovered, "stack", string(debug.Stack()))
				writeAPIError(w, http.StatusInternalServerError, "internal_error", "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}
