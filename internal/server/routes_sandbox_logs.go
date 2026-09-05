package server

import (
	"fmt"
	"github.com/qiyuehuajin/agentspool/internal/core"
	"github.com/qiyuehuajin/agentspool/internal/logstream"
	"net/http"
	"strconv"
	"strings"
)

func (s *Server) handleSandboxOperationLogs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("operationID")
	var sandboxID string
	_ = s.store.View(func(state *core.State) error {
		if op := state.SandboxOperations[id]; op != nil {
			sandboxID = op.SandboxID
		}
		return nil
	})
	if sandboxID == "" {
		writeAPIError(w, 404, "operation_not_found", "sandbox operation not found")
		return
	}
	if _, ok := s.authenticateSandbox(w, r, sandboxID); !ok {
		return
	}
	after := int64(0)
	if raw := r.URL.Query().Get("after"); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value < 0 {
			writeAPIError(w, 400, "invalid_log_offset", "after must be a non-negative byte offset")
			return
		}
		after = value
	}
	stream := r.URL.Query().Get("stream")
	if stream == "" {
		stream = "stdout"
	}
	page, err := s.operationLogs.Read(id, stream, after, parseLimit(r, logstream.PageBytes, logstream.PageBytes))
	if err != nil {
		writeAPIError(w, 400, "log_read_failed", err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"operation_id": id, "stream": stream, "offset": page.Offset, "next_after": page.NextAfter, "first_offset": page.FirstOffset, "end_offset": page.EndOffset, "truncated": page.Truncated, "data": string(page.Data), "data_base64": page.Data})
}
func (s *Server) handleSandboxOperationLogsUpload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("operationID")
	var nodeID string
	_ = s.store.View(func(state *core.State) error {
		if op := state.SandboxOperations[id]; op != nil {
			nodeID = op.NodeID
		}
		return nil
	})
	if nodeID == "" {
		writeAPIError(w, 404, "operation_not_found", "sandbox operation not found")
		return
	}
	if _, ok := s.authenticateNode(w, r, nodeID); !ok {
		return
	}
	var request struct {
		Stream    string `json:"stream"`
		Offset    int64  `json:"offset"`
		Data      []byte `json:"data_base64"`
		Truncated bool   `json:"truncated"`
	}
	if !decodeJSON(w, r, 2*logstream.PageBytes, &request) {
		return
	}
	if request.Offset < 0 || len(request.Data) > logstream.PageBytes || (request.Stream != "stdout" && request.Stream != "stderr") {
		writeAPIError(w, 400, "invalid_log_chunk", "invalid stream, offset or chunk size")
		return
	}
	token := strings.TrimSpace(r.Header.Get("X-AgentsPool-Sandbox-Operation-Token"))
	var end int64
	claimRejected := false
	// Hold the read lock through append so completion/prune cannot remove this
	// operation between authorization and its durable write.
	err := s.store.View(func(state *core.State) error {
		op := state.SandboxOperations[id]
		if op == nil || op.Terminal() || token == "" || !core.SecureHashEqual(op.ClaimTokenHash, token) {
			claimRejected = true
			return fmt.Errorf("operation claim is not active")
		}
		var err error
		end, err = s.operationLogs.AppendAt(id, request.Stream, request.Offset, request.Data, request.Truncated)
		return err
	})
	if err != nil {
		status := http.StatusInternalServerError
		if claimRejected {
			status = http.StatusConflict
		}
		writeAPIError(w, status, "log_upload_failed", err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"next_after": end})
}
