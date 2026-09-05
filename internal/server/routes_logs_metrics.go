package server

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/qiyuehuajin/agentspool/internal/core"
)

func (s *Server) handleTaskLogs(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	taskID := r.PathValue("taskID")
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	limit := parseLimit(r, 1000, 5000)
	var exists bool
	executions := []*core.Execution{}
	_ = s.store.View(func(state *core.State) error {
		_, exists = state.Tasks[taskID]
		if exists {
			for _, execution := range state.Executions {
				if execution.TaskID == taskID {
					copy := cloneViaJSON(*execution)
					executions = append(executions, &copy)
				}
			}
		}
		return nil
	})
	if !exists {
		writeAPIError(w, http.StatusNotFound, "task_not_found", "task not found")
		return
	}
	sort.Slice(executions, func(i, j int) bool { return executions[i].Attempt > executions[j].Attempt })
	if len(executions) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"execution_id": "", "events": []any{}, "next_after": after})
		return
	}
	executionID := strings.TrimSpace(r.URL.Query().Get("execution_id"))
	if executionID == "" {
		executionID = executions[0].ID
	}
	found := false
	for _, execution := range executions {
		if execution.ID == executionID {
			found = true
			break
		}
	}
	if !found {
		writeAPIError(w, http.StatusBadRequest, "execution_not_in_task", "execution does not belong to task")
		return
	}
	events, err := s.logs.Read(executionID, after, limit)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "log_read_failed", err.Error())
		return
	}
	nextAfter := after
	for _, event := range events {
		if event.Sequence > nextAfter {
			nextAfter = event.Sequence
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"execution_id": executionID, "events": events, "next_after": nextAfter})
}

func (s *Server) handleExecutionLogs(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	executionID := r.PathValue("executionID")
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	limit := parseLimit(r, 1000, 5000)
	var exists bool
	_ = s.store.View(func(state *core.State) error {
		_, exists = state.Executions[executionID]
		return nil
	})
	if !exists {
		writeAPIError(w, http.StatusNotFound, "execution_not_found", "execution not found")
		return
	}
	events, err := s.logs.Read(executionID, after, limit)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "log_read_failed", err.Error())
		return
	}
	nextAfter := after
	for _, event := range events {
		if event.Sequence > nextAfter {
			nextAfter = event.Sequence
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"execution_id": executionID, "events": events, "next_after": nextAfter})
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.PublicMetrics && !s.requireAdmin(w, r) {
		return
	}
	state, err := s.store.Snapshot()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "snapshot_failed", err.Error())
		return
	}
	nodeCounts := map[string]int{}
	taskCounts := map[string]int{}
	for _, node := range state.Nodes {
		nodeCounts[node.Status]++
	}
	for _, task := range state.Tasks {
		taskCounts[task.Status]++
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = fmt.Fprintln(w, "# HELP agentspool_up Whether the control plane is running.")
	_, _ = fmt.Fprintln(w, "# TYPE agentspool_up gauge")
	_, _ = fmt.Fprintln(w, "agentspool_up 1")
	_, _ = fmt.Fprintln(w, "# HELP agentspool_nodes Current nodes by status.")
	_, _ = fmt.Fprintln(w, "# TYPE agentspool_nodes gauge")
	for status, count := range nodeCounts {
		_, _ = fmt.Fprintf(w, "agentspool_nodes{status=%q} %d\n", status, count)
	}
	_, _ = fmt.Fprintln(w, "# HELP agentspool_tasks Current tasks by status.")
	_, _ = fmt.Fprintln(w, "# TYPE agentspool_tasks gauge")
	for status, count := range taskCounts {
		_, _ = fmt.Fprintf(w, "agentspool_tasks{status=%q} %d\n", status, count)
	}
	writeCounter := func(name, help string, value uint64) {
		_, _ = fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", name, help, name, name, value)
	}
	writeCounter("agentspool_http_requests_total", "HTTP requests received.", s.metrics.RequestsTotal.Load())
	writeCounter("agentspool_claims_total", "Tasks claimed by workers.", s.metrics.ClaimsTotal.Load())
	writeCounter("agentspool_completions_total", "Successful task completions.", s.metrics.CompletionsTotal.Load())
	writeCounter("agentspool_failures_total", "Task execution failures.", s.metrics.FailuresTotal.Load())
	writeCounter("agentspool_lease_expirations_total", "Expired execution leases.", s.metrics.LeaseExpirations.Load())
	writeCounter("agentspool_artifacts_uploaded_total", "Artifacts uploaded.", s.metrics.ArtifactsUploaded.Load())
	writeCounter("agentspool_artifact_bytes_uploaded_total", "Artifact bytes uploaded.", s.metrics.BytesUploaded.Load())
	writeCounter("agentspool_authentication_failures_total", "Rejected authentication attempts.", s.metrics.AuthenticationFails.Load())
}
