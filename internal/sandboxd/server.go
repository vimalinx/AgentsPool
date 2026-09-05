package sandboxd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/qiyuehuajin/agentspool/internal/core"
	"github.com/qiyuehuajin/agentspool/internal/logstream"
)

type Config struct {
	SocketPath     string
	DataDir        string
	WorkerUser     string
	MaxOutputBytes int64
	TransferRoot   string
	LogRetainBytes int64
}

type OperationRequest struct {
	Operation    core.SandboxOperation `json:"operation"`
	Sandbox      core.Sandbox          `json:"sandbox"`
	Resources    core.ResourceRequest  `json:"resources"`
	TransferPath string                `json:"transfer_path,omitempty"`
}

type OperationResult struct {
	Success         bool   `json:"success"`
	ExitCode        *int   `json:"exit_code,omitempty"`
	Stdout          string `json:"stdout,omitempty"`
	Stderr          string `json:"stderr,omitempty"`
	Error           string `json:"error,omitempty"`
	OutputTruncated bool   `json:"output_truncated,omitempty"`
}

type OperationStatus struct {
	OperationID string `json:"operation_id"`
	Running     bool   `json:"running"`
	LoadState   string `json:"load_state,omitempty"`
	ActiveState string `json:"active_state,omitempty"`
	SubState    string `json:"sub_state,omitempty"`
}

type Server struct {
	cfg       Config
	logger    *slog.Logger
	uid       int
	gid       int
	server    *http.Server
	logStores sync.Map
}

var safeID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

func New(cfg Config, logger *slog.Logger) (*Server, error) {
	if os.Geteuid() != 0 {
		return nil, errors.New("sandboxd must run as root")
	}
	if cfg.SocketPath == "" || !filepath.IsAbs(cfg.SocketPath) {
		return nil, errors.New("sandbox socket path must be absolute")
	}
	if cfg.DataDir == "" || !filepath.IsAbs(cfg.DataDir) {
		return nil, errors.New("sandbox data directory must be absolute")
	}
	if filepath.Clean(cfg.DataDir) == string(filepath.Separator) {
		return nil, errors.New("sandbox data directory must not be the filesystem root")
	}
	if cfg.WorkerUser == "" {
		cfg.WorkerUser = "agentspool"
	}
	if cfg.MaxOutputBytes < 0 {
		return nil, errors.New("sandbox output limit cannot be negative")
	}
	if cfg.LogRetainBytes < 0 {
		return nil, errors.New("sandbox log retention cannot be negative")
	}
	if cfg.TransferRoot == "" || !filepath.IsAbs(cfg.TransferRoot) {
		return nil, errors.New("sandbox transfer root must be absolute")
	}
	if filepath.Clean(cfg.TransferRoot) == string(filepath.Separator) {
		return nil, errors.New("sandbox transfer root must not be the filesystem root")
	}
	account, err := user.Lookup(cfg.WorkerUser)
	if err != nil {
		return nil, fmt.Errorf("lookup worker user: %w", err)
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil {
		return nil, fmt.Errorf("parse worker uid: %w", err)
	}
	gid, err := strconv.Atoi(account.Gid)
	if err != nil {
		return nil, fmt.Errorf("parse worker gid: %w", err)
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{cfg: cfg, logger: logger, uid: uid, gid: gid}, nil
}

func (s *Server) Run(ctx context.Context) error {
	if err := os.MkdirAll(s.cfg.DataDir, 0o710); err != nil {
		return fmt.Errorf("create sandbox data directory: %w", err)
	}
	if err := os.Chmod(s.cfg.DataDir, 0o710); err != nil {
		return fmt.Errorf("protect sandbox data directory: %w", err)
	}
	if err := os.Chown(s.cfg.DataDir, 0, s.gid); err != nil {
		return fmt.Errorf("set sandbox data directory ownership: %w", err)
	}
	if err := os.MkdirAll(s.cfg.TransferRoot, 0o700); err != nil {
		return fmt.Errorf("create sandbox transfer root: %w", err)
	}
	if err := os.Chmod(s.cfg.TransferRoot, 0o700); err != nil {
		return fmt.Errorf("protect sandbox transfer root: %w", err)
	}
	if err := os.Chown(s.cfg.TransferRoot, s.uid, s.gid); err != nil {
		return fmt.Errorf("set sandbox transfer root ownership: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.cfg.SocketPath), 0o755); err != nil {
		return fmt.Errorf("create socket directory: %w", err)
	}
	if info, err := os.Lstat(s.cfg.SocketPath); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("refusing to replace non-socket path %s", s.cfg.SocketPath)
		}
		if err := os.Remove(s.cfg.SocketPath); err != nil {
			return fmt.Errorf("remove stale socket: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	listener, err := net.Listen("unix", s.cfg.SocketPath)
	if err != nil {
		return fmt.Errorf("listen on sandbox socket: %w", err)
	}
	defer listener.Close()
	defer os.Remove(s.cfg.SocketPath)
	if err := os.Chown(s.cfg.SocketPath, 0, s.gid); err != nil {
		return fmt.Errorf("set socket ownership: %w", err)
	}
	if err := os.Chmod(s.cfg.SocketPath, 0o660); err != nil {
		return fmt.Errorf("set socket permissions: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/ping", s.handlePing)
	mux.HandleFunc("POST /v1/operations", s.handleOperation)
	mux.HandleFunc("GET /v1/operations/{operationID}", s.handleOperationStatus)
	mux.HandleFunc("POST /v1/operations/{operationID}/cancel", s.handleOperationCancel)
	mux.HandleFunc("GET /v1/sandboxes/{sandboxID}/operations/{operationID}/logs", s.handleOperationLogs)
	s.server = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 0, IdleTimeout: 30 * time.Second}
	errCh := make(chan error, 1)
	go func() { errCh <- s.server.Serve(listener) }()
	s.logger.Info("AgentsPool sandbox helper listening", "socket", s.cfg.SocketPath, "data_dir", s.cfg.DataDir, "worker_uid", s.uid, "worker_gid", s.gid)
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.server.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func (s *Server) handlePing(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "backend": "systemd-bwrap", "gpu_access": false})
}

func (s *Server) handleOperationCancel(w http.ResponseWriter, r *http.Request) {
	operationID := r.PathValue("operationID")
	if !safeID.MatchString(operationID) || !strings.HasPrefix(operationID, "sbxop_") {
		writeError(w, http.StatusBadRequest, "invalid sandbox operation id")
		return
	}
	if err := stopOperationUnit(r.Context(), operationID); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"operation_id": operationID, "stopped": true})
}

func (s *Server) handleOperationStatus(w http.ResponseWriter, r *http.Request) {
	operationID := r.PathValue("operationID")
	if !safeID.MatchString(operationID) || !strings.HasPrefix(operationID, "sbxop_") {
		writeError(w, http.StatusBadRequest, "invalid sandbox operation id")
		return
	}
	unit := "agentspool-sbx-" + unitSuffix(operationID) + ".service"
	command := exec.CommandContext(r.Context(), "systemctl", "show", unit, "--no-pager", "--property=LoadState,ActiveState,SubState")
	output, err := command.CombinedOutput()
	status := OperationStatus{OperationID: operationID}
	for _, line := range strings.Split(string(output), "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if !found {
			continue
		}
		switch key {
		case "LoadState":
			status.LoadState = value
		case "ActiveState":
			status.ActiveState = value
		case "SubState":
			status.SubState = value
		}
	}
	status.Running = status.LoadState == "loaded" && (status.ActiveState == "active" || status.ActiveState == "activating" || status.ActiveState == "reloading" || status.ActiveState == "deactivating")
	if err != nil && status.LoadState != "not-found" {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("inspect sandbox operation unit: %v", err))
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleOperation(w http.ResponseWriter, r *http.Request) {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var request OperationRequest
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeError(w, http.StatusBadRequest, "request must contain one JSON value")
		return
	}
	result, err := s.apply(r.Context(), request)
	if err != nil {
		result.Success = false
		result.Error = err.Error()
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) apply(ctx context.Context, request OperationRequest) (OperationResult, error) {
	if !safeID.MatchString(request.Sandbox.ID) || !strings.HasPrefix(request.Sandbox.ID, "sbx_") {
		return OperationResult{}, errors.New("invalid sandbox id")
	}
	if request.Operation.SandboxID != request.Sandbox.ID || request.Operation.NodeID != request.Sandbox.NodeID {
		return OperationResult{}, errors.New("operation and sandbox identity mismatch")
	}
	if !safeID.MatchString(request.Operation.ID) || !strings.HasPrefix(request.Operation.ID, "sbxop_") {
		return OperationResult{}, errors.New("invalid sandbox operation id")
	}
	if request.Resources.CPUMillis < 10 || request.Resources.MemoryMB < 16 || request.Resources.DiskMB < 32 {
		return OperationResult{}, errors.New("invalid sandbox resource limits")
	}
	unlock, err := s.lockSandbox(ctx, request.Sandbox.ID)
	if err != nil {
		return OperationResult{}, err
	}
	defer unlock()
	switch request.Operation.Kind {
	case "ensure":
		return OperationResult{Success: true}, s.ensure(request)
	case "resize":
		if err := s.ensure(request); err != nil {
			return OperationResult{}, err
		}
		return OperationResult{Success: true}, nil
	case "exec":
		return s.execute(ctx, request)
	case "put":
		return OperationResult{Success: true}, s.put(request)
	case "get":
		return OperationResult{Success: true}, s.get(request)
	case "stop":
		return OperationResult{Success: true}, s.stop(ctx, request.Sandbox.ID)
	case "destroy":
		if err := s.stop(ctx, request.Sandbox.ID); err != nil {
			return OperationResult{}, err
		}
		return OperationResult{Success: true}, s.destroy(request.Sandbox.ID)
	default:
		return OperationResult{}, fmt.Errorf("unsupported sandbox operation %q", request.Operation.Kind)
	}
}

func (s *Server) sandboxPaths(id string) (root, workspace, home string) {
	root = filepath.Join(s.cfg.DataDir, id)
	mountRoot := filepath.Join(root, "rootfs")
	return root, filepath.Join(mountRoot, "workspace"), filepath.Join(mountRoot, "home")
}

func (s *Server) ensure(request OperationRequest) error {
	root, workspace, home := s.sandboxPaths(request.Sandbox.ID)
	if err := os.MkdirAll(root, 0o710); err != nil {
		return fmt.Errorf("create sandbox root: %w", err)
	}
	if err := os.Chown(root, 0, s.gid); err != nil {
		return fmt.Errorf("set sandbox root ownership: %w", err)
	}
	imagePath := filepath.Join(root, "storage.ext4")
	mountRoot := filepath.Join(root, "rootfs")
	if err := os.MkdirAll(mountRoot, 0o750); err != nil {
		return fmt.Errorf("create sandbox mountpoint: %w", err)
	}
	if err := os.Chown(mountRoot, 0, s.gid); err != nil {
		return fmt.Errorf("set sandbox mountpoint ownership: %w", err)
	}
	wantedBytes := request.Resources.DiskMB << 20
	imageInfo, statErr := os.Stat(imagePath)
	imageExisted := statErr == nil
	switch {
	case errors.Is(statErr, os.ErrNotExist):
		image, openErr := os.OpenFile(imagePath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if openErr != nil {
			return fmt.Errorf("create sandbox disk image: %w", openErr)
		}
		if truncateErr := image.Truncate(wantedBytes); truncateErr != nil {
			_ = image.Close()
			_ = os.Remove(imagePath)
			return fmt.Errorf("size sandbox disk image: %w", truncateErr)
		}
		if closeErr := image.Close(); closeErr != nil {
			_ = os.Remove(imagePath)
			return closeErr
		}
		if output, formatErr := exec.Command("mkfs.ext4", "-q", "-F", "-m", "0", imagePath).CombinedOutput(); formatErr != nil {
			_ = os.Remove(imagePath)
			return fmt.Errorf("format sandbox disk image: %w: %s", formatErr, strings.TrimSpace(string(output)))
		}
	case statErr != nil:
		return fmt.Errorf("inspect sandbox disk image: %w", statErr)
	case imageInfo.Size() > wantedBytes:
		return fmt.Errorf("persistent sandbox disk cannot shrink from %dMB to %dMB", imageInfo.Size()>>20, request.Resources.DiskMB)
	case imageInfo.Size() < wantedBytes:
		if err := unmountIfMounted(mountRoot); err != nil {
			return err
		}
		if err := os.Truncate(imagePath, wantedBytes); err != nil {
			return fmt.Errorf("grow sandbox disk image: %w", err)
		}
	}
	// Recheck and resize every existing, currently unmounted image. This makes a
	// partially completed grow retryable after a helper or host interruption.
	if imageExisted && !isMountpoint(mountRoot) {
		check := exec.Command("e2fsck", "-pf", imagePath)
		if output, checkErr := check.CombinedOutput(); checkErr != nil {
			var exitErr *exec.ExitError
			if !errors.As(checkErr, &exitErr) || exitErr.ExitCode() != 1 {
				return fmt.Errorf("check sandbox disk image: %w: %s", checkErr, strings.TrimSpace(string(output)))
			}
		}
		if output, resizeErr := exec.Command("resize2fs", imagePath).CombinedOutput(); resizeErr != nil {
			return fmt.Errorf("resize sandbox disk image: %w: %s", resizeErr, strings.TrimSpace(string(output)))
		}
	}
	if !isMountpoint(mountRoot) {
		if output, mountErr := exec.Command("mount", "-o", "loop,nodev,nosuid", imagePath, mountRoot).CombinedOutput(); mountErr != nil {
			return fmt.Errorf("mount sandbox disk image: %w: %s", mountErr, strings.TrimSpace(string(output)))
		}
	}
	for _, path := range []string{workspace, home} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return fmt.Errorf("create persistent volume: %w", err)
		}
		if err := os.Chown(path, s.uid, s.gid); err != nil {
			return fmt.Errorf("set persistent volume ownership: %w", err)
		}
		if err := os.Chmod(path, 0o700); err != nil {
			return fmt.Errorf("set persistent volume mode: %w", err)
		}
	}
	metadata := map[string]any{
		"sandbox_id": request.Sandbox.ID, "node_id": request.Sandbox.NodeID,
		"resources": request.Resources, "permissions": request.Sandbox.Permissions,
		"updated_at": time.Now().UTC(), "gpu_access": false,
	}
	encoded, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(root, ".metadata-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = tmp.Close(); _ = os.Remove(tmpName) }
	if err := tmp.Chmod(0o640); err != nil {
		cleanup()
		return err
	}
	if _, err := tmp.Write(encoded); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, filepath.Join(root, "metadata.json")); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

func (s *Server) execute(parent context.Context, request OperationRequest) (OperationResult, error) {
	if !request.Sandbox.Permissions.Exec {
		return OperationResult{}, errors.New("sandbox does not grant exec permission")
	}
	if request.Operation.Exec == nil || len(request.Operation.Exec.Command) == 0 {
		return OperationResult{}, errors.New("exec operation has no command")
	}
	if err := s.ensure(request); err != nil {
		return OperationResult{}, err
	}
	root, workspace, home := s.sandboxPaths(request.Sandbox.ID)
	_ = root
	workingDir := request.Operation.Exec.WorkingDir
	if workingDir == "" {
		workingDir = "/workspace"
	}
	workingDir = filepath.ToSlash(filepath.Clean(workingDir))
	if workingDir != "/workspace" && workingDir != "/home/agent" && !strings.HasPrefix(workingDir, "/workspace/") && !strings.HasPrefix(workingDir, "/home/agent/") {
		return OperationResult{}, errors.New("working directory escapes persistent volumes")
	}
	ctx := parent
	cancel := func() {}
	if request.Operation.Exec.TimeoutSeconds > 0 {
		ctx, cancel = context.WithTimeout(parent, core.SecondsDuration(request.Operation.Exec.TimeoutSeconds))
	}
	defer cancel()

	bwrapArgs := []string{"--unshare-ipc", "--unshare-pid", "--unshare-uts", "--unshare-cgroup", "--new-session", "--die-with-parent"}
	if !request.Sandbox.Permissions.Network {
		bwrapArgs = append(bwrapArgs, "--unshare-net")
	}
	for _, path := range []string{"/usr", "/bin", "/lib", "/lib64"} {
		if _, err := os.Stat(path); err == nil {
			bwrapArgs = append(bwrapArgs, "--ro-bind", path, path)
		}
	}
	bwrapArgs = append(bwrapArgs, "--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp", "--dir", "/etc", "--dir", "/home")
	if request.Sandbox.Permissions.Network {
		for _, path := range []string{"/etc/resolv.conf", "/etc/hosts", "/etc/ssl", "/etc/ca-certificates"} {
			if _, err := os.Stat(path); err == nil {
				bwrapArgs = append(bwrapArgs, "--ro-bind", path, path)
			}
		}
	}
	switch {
	case request.Sandbox.Permissions.DataRead && request.Sandbox.Permissions.DataWrite:
		bwrapArgs = append(bwrapArgs, "--bind", workspace, "/workspace", "--bind", home, "/home/agent")
	case request.Sandbox.Permissions.DataRead:
		bwrapArgs = append(bwrapArgs, "--ro-bind", workspace, "/workspace", "--ro-bind", home, "/home/agent")
	default:
		bwrapArgs = append(bwrapArgs, "--tmpfs", "/workspace", "--tmpfs", "/home/agent")
	}
	bwrapArgs = append(bwrapArgs, "--chdir", workingDir)
	bwrapArgs = append(bwrapArgs, "/usr/bin/setpriv", "--reuid", strconv.Itoa(s.uid), "--regid", strconv.Itoa(s.gid), "--clear-groups", "--inh-caps=-all", "--ambient-caps=-all", "--bounding-set=-all", "--no-new-privs")
	bwrapArgs = append(bwrapArgs, "/usr/bin/env", "-i", "HOME=/home/agent", "USER=agent", "LOGNAME=agent", "PATH=/usr/local/bin:/usr/bin:/bin", "LANG=C.UTF-8")
	keys := make([]string, 0, len(request.Operation.Exec.Env))
	for key := range request.Operation.Exec.Env {
		keys = append(keys, key)
	}
	sortStrings(keys)
	for _, key := range keys {
		if key == "HOME" || key == "PATH" || key == "USER" || key == "LOGNAME" || strings.HasPrefix(key, "LD_") || strings.ContainsAny(key, "=\x00") || strings.ContainsRune(request.Operation.Exec.Env[key], 0) {
			return OperationResult{}, fmt.Errorf("environment variable %q is not allowed", key)
		}
		bwrapArgs = append(bwrapArgs, key+"="+request.Operation.Exec.Env[key])
	}
	bwrapArgs = append(bwrapArgs, request.Operation.Exec.Command...)

	unit := "agentspool-sbx-" + unitSuffix(request.Operation.ID)
	cpuQuota := fmt.Sprintf("%.2f%%", float64(request.Resources.CPUMillis)/10.0)
	systemdArgs := []string{
		"--quiet", "--wait", "--pipe", "--collect", "--unit", unit,
		"-p", "MemoryMax=" + strconv.FormatInt(request.Resources.MemoryMB, 10) + "M",
		"-p", "CPUQuota=" + cpuQuota,
		"-p", "TasksMax=infinity", "-p", "NoNewPrivileges=yes",
	}
	if request.Operation.Exec.TimeoutSeconds > 0 {
		systemdArgs = append(systemdArgs, "-p", "RuntimeMaxSec="+strconv.Itoa(request.Operation.Exec.TimeoutSeconds))
	}
	systemdArgs = append(systemdArgs, "-p", "TimeoutStopSec=5s", "bwrap")
	systemdArgs = append(systemdArgs, bwrapArgs...)
	command := exec.CommandContext(ctx, "systemd-run", systemdArgs...)
	logs := s.operationLogs(request.Sandbox.ID)
	if err := os.MkdirAll(filepath.Join(logs.Dir, request.Operation.ID), 0o700); err != nil {
		return OperationResult{}, err
	}
	command.WaitDelay = 20 * time.Second
	command.Cancel = func() error {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer stopCancel()
		if stopErr := stopOperationUnit(stopCtx, request.Operation.ID); stopErr != nil {
			_ = command.Process.Kill()
			return stopErr
		}
		return command.Process.Kill()
	}
	command.Stdout = logs.Writer(request.Operation.ID, "stdout")
	command.Stderr = logs.Writer(request.Operation.ID, "stderr")
	err := command.Run()
	if ctx.Err() != nil {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 15*time.Second)
		stopErr := stopOperationUnit(stopCtx, request.Operation.ID)
		stopCancel()
		if stopErr != nil {
			return OperationResult{}, stopErr
		}
	}
	exitCode := 0
	if err != nil {
		exitCode = -1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
	}
	previewLimit := logstream.PageBytes
	if s.cfg.MaxOutputBytes > 0 && s.cfg.MaxOutputBytes < int64(previewLimit) {
		previewLimit = int(s.cfg.MaxOutputBytes)
	}
	stdout, stdoutTruncated, stdoutErr := logs.Tail(request.Operation.ID, "stdout", previewLimit)
	stderr, stderrTruncated, stderrErr := logs.Tail(request.Operation.ID, "stderr", previewLimit)
	if stdoutErr != nil {
		return OperationResult{}, stdoutErr
	}
	if stderrErr != nil {
		return OperationResult{}, stderrErr
	}
	result := OperationResult{Success: err == nil, ExitCode: &exitCode, Stdout: stdout, Stderr: stderr, OutputTruncated: stdoutTruncated || stderrTruncated}
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			result.Error = "sandbox execution timed out"
			return result, errors.New(result.Error)
		}
		result.Error = err.Error()
		return result, err
	}
	return result, nil
}

func (s *Server) put(request OperationRequest) error {
	if !request.Sandbox.Permissions.DataWrite || request.Operation.File == nil {
		return errors.New("sandbox does not grant data_write permission")
	}
	if err := s.ensure(request); err != nil {
		return err
	}
	source, err := s.validTransferPath(request.TransferPath, true)
	if err != nil {
		return err
	}
	destination, err := s.persistentFilePath(request.Sandbox.ID, request.Operation.File.Path, true)
	if err != nil {
		return err
	}
	mode := os.FileMode(0o600)
	if request.Operation.File.Executable {
		mode = 0o700
	}
	return copyNoFollow(source, destination, mode, s.uid, s.gid)
}

func (s *Server) get(request OperationRequest) error {
	if !request.Sandbox.Permissions.DataRead || request.Operation.File == nil {
		return errors.New("sandbox does not grant data_read permission")
	}
	if err := s.ensure(request); err != nil {
		return err
	}
	source, err := s.persistentFilePath(request.Sandbox.ID, request.Operation.File.Path, false)
	if err != nil {
		return err
	}
	destination, err := s.validTransferPath(request.TransferPath, false)
	if err != nil {
		return err
	}
	return copyNoFollow(source, destination, 0o600, s.uid, s.gid)
}

func (s *Server) validTransferPath(value string, requireExisting bool) (string, error) {
	cleaned := filepath.Clean(value)
	root := filepath.Clean(s.cfg.TransferRoot)
	if cleaned == root || !strings.HasPrefix(cleaned, root+string(filepath.Separator)) {
		return "", errors.New("transfer path escapes the fixed transfer root")
	}
	parent := filepath.Dir(cleaned)
	if info, err := os.Lstat(parent); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("transfer parent is not a safe directory")
	}
	info, err := os.Lstat(cleaned)
	if requireExisting {
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("transfer source must be a regular file")
		}
	} else if err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("transfer destination is not a regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return cleaned, nil
}

func (s *Server) persistentFilePath(sandboxID, virtualPath string, createParents bool) (string, error) {
	_, workspace, home := s.sandboxPaths(sandboxID)
	var base, relative string
	switch {
	case strings.HasPrefix(virtualPath, "/workspace/"):
		base, relative = workspace, strings.TrimPrefix(virtualPath, "/workspace/")
	case strings.HasPrefix(virtualPath, "/home/agent/"):
		base, relative = home, strings.TrimPrefix(virtualPath, "/home/agent/")
	default:
		return "", errors.New("sandbox file path is outside persistent volumes")
	}
	components := strings.Split(filepath.Clean(filepath.FromSlash(relative)), string(filepath.Separator))
	if len(components) == 0 || components[0] == "." || components[0] == ".." {
		return "", errors.New("invalid sandbox file path")
	}
	current := base
	for _, component := range components[:len(components)-1] {
		if component == "" || component == "." || component == ".." {
			return "", errors.New("invalid sandbox file path component")
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) && createParents {
			if err := os.Mkdir(current, 0o700); err != nil {
				return "", err
			}
			if err := os.Chown(current, s.uid, s.gid); err != nil {
				return "", err
			}
			continue
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("sandbox file path crosses a non-directory or symlink")
		}
	}
	finalPath := filepath.Join(current, components[len(components)-1])
	if info, err := os.Lstat(finalPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return "", errors.New("sandbox file target is not a regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) || !createParents {
		if err != nil {
			return "", err
		}
	}
	return finalPath, nil
}

func copyNoFollow(source, destination string, mode os.FileMode, uid, gid int) error {
	sourceFD, err := syscall.Open(source, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open transfer source: %w", err)
	}
	input := os.NewFile(uintptr(sourceFD), source)
	defer input.Close()
	destinationFD, err := syscall.Open(destination, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_TRUNC|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, uint32(mode.Perm()))
	if err != nil {
		return fmt.Errorf("open transfer destination: %w", err)
	}
	output := os.NewFile(uintptr(destinationFD), destination)
	ok := false
	defer func() {
		_ = output.Close()
		if !ok {
			_ = os.Remove(destination)
		}
	}()
	if _, err := io.Copy(output, input); err != nil {
		return err
	}
	if err := output.Sync(); err != nil {
		return err
	}
	if err := output.Chmod(mode); err != nil {
		return err
	}
	if err := output.Chown(uid, gid); err != nil {
		return err
	}
	ok = true
	return nil
}

func (s *Server) destroy(id string) error {
	if !safeID.MatchString(id) || !strings.HasPrefix(id, "sbx_") {
		return errors.New("invalid sandbox id")
	}
	root, _, _ := s.sandboxPaths(id)
	parent := filepath.Clean(s.cfg.DataDir) + string(filepath.Separator)
	if !strings.HasPrefix(filepath.Clean(root)+string(filepath.Separator), parent) {
		return errors.New("sandbox path escapes data directory")
	}
	if err := unmountIfMounted(filepath.Join(root, "rootfs")); err != nil {
		return err
	}
	if err := os.RemoveAll(root); err != nil {
		return fmt.Errorf("remove sandbox data: %w", err)
	}
	s.logStores.Delete(id)
	return nil
}

func isMountpoint(path string) bool {
	return exec.Command("mountpoint", "-q", path).Run() == nil
}

func unmountIfMounted(path string) error {
	if !isMountpoint(path) {
		return nil
	}
	if output, err := exec.Command("umount", path).CombinedOutput(); err != nil {
		return fmt.Errorf("unmount sandbox disk image: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func unitSuffix(id string) string {
	value := strings.TrimPrefix(id, "sbxop_")
	if len(value) > 48 {
		value = value[len(value)-48:]
	}
	return strings.ToLower(value)
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"success": false, "error": message})
}
