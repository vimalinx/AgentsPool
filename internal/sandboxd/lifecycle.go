package sandboxd

import (
	"context"
	"errors"
	"fmt"
	"github.com/vimalinx/AgentsPool/internal/logstream"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func (s *Server) lockSandbox(ctx context.Context, id string) (func(), error) {
	dir := filepath.Join(s.cfg.DataDir, "locks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(dir, id+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN); _ = file.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			file.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			file.Close()
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}
func stopOperationUnit(ctx context.Context, id string) error {
	unit := "agentspool-sbx-" + unitSuffix(id) + ".service"
	output, err := exec.CommandContext(ctx, "systemctl", "stop", unit).CombinedOutput()
	if err != nil && !strings.Contains(string(output), "not loaded") && !strings.Contains(string(output), "not found") {
		return fmt.Errorf("stop sandbox unit: %w: %s", err, strings.TrimSpace(string(output)))
	}
	output, err = exec.CommandContext(ctx, "systemctl", "show", unit, "--property=ActiveState", "--value").CombinedOutput()
	state := strings.TrimSpace(string(output))
	if (err == nil && (state == "inactive" || state == "failed")) || strings.Contains(state, "not found") || strings.Contains(state, "not loaded") {
		return nil
	}
	return fmt.Errorf("sandbox unit termination not confirmed: state=%q, error=%v", state, err)
}
func (s *Server) stop(ctx context.Context, id string) error {
	root, _, _ := s.sandboxPaths(id)
	entries, err := os.ReadDir(filepath.Join(root, "operations"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !safeID.MatchString(entry.Name()) || !strings.HasPrefix(entry.Name(), "sbxop_") {
			continue
		}
		if err := stopOperationUnit(ctx, entry.Name()); err != nil {
			return err
		}
	}
	return unmountIfMounted(filepath.Join(root, "rootfs"))
}
func (s *Server) operationLogs(sandboxID string) *logstream.Store {
	root, _, _ := s.sandboxPaths(sandboxID)
	value, _ := s.logStores.LoadOrStore(sandboxID, &logstream.Store{Dir: filepath.Join(root, "operations"), RetainBytes: s.cfg.LogRetainBytes})
	return value.(*logstream.Store)
}
func (s *Server) handleOperationLogs(w http.ResponseWriter, r *http.Request) {
	sandboxID, operationID := r.PathValue("sandboxID"), r.PathValue("operationID")
	if !safeID.MatchString(sandboxID) || !strings.HasPrefix(sandboxID, "sbx_") || !safeID.MatchString(operationID) || !strings.HasPrefix(operationID, "sbxop_") {
		writeError(w, http.StatusBadRequest, "invalid sandbox operation identity")
		return
	}
	after, err := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	if err != nil || after < 0 {
		writeError(w, http.StatusBadRequest, "invalid log offset")
		return
	}
	stream := r.URL.Query().Get("stream")
	page, err := s.operationLogs(sandboxID).Read(operationID, stream, after, logstream.PageBytes)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, page)
}
