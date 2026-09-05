package server

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/qiyuehuajin/agentspool/internal/core"
)

func (s *Server) handleAdminArtifactUpload(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	name := r.URL.Query().Get("name")
	if name == "" {
		name = r.Header.Get("X-Artifact-Name")
	}
	artifact, err := s.ingestArtifact(w, r, name, r.URL.Query().Get("media_type"), "", "", nil)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "artifact_upload_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"artifact": artifact})
}

func (s *Server) handleLeaseArtifactUpload(w http.ResponseWriter, r *http.Request) {
	leaseID := r.PathValue("leaseID")
	lease, ok := s.authenticateLease(w, r, leaseID)
	if !ok {
		return
	}
	active := false
	_ = s.store.View(func(state *core.State) error {
		current := state.Leases[leaseID]
		active = current != nil && current.Status == core.LeaseActive && current.ExpiresAt.After(time.Now().UTC())
		return nil
	})
	if !active {
		writeAPIError(w, http.StatusConflict, "lease_not_active", "lease is no longer active")
		return
	}
	name := r.URL.Query().Get("name")
	if name == "" {
		name = r.Header.Get("X-Artifact-Name")
	}
	metadata := map[string]string{}
	if relativePath := strings.TrimSpace(r.URL.Query().Get("relative_path")); relativePath != "" {
		metadata["relative_path"] = relativePath
	}
	artifact, err := s.ingestArtifact(w, r, name, r.URL.Query().Get("media_type"), lease.TaskID, lease.ExecutionID, metadata)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "artifact_upload_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"artifact": artifact})
}

func (s *Server) handleArtifactsList(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	limit := parseLimit(r, 100, 1000)
	artifacts := []*core.Artifact{}
	_ = s.store.View(func(state *core.State) error {
		for _, artifact := range state.Artifacts {
			copy := cloneViaJSON(*artifact)
			artifacts = append(artifacts, &copy)
		}
		return nil
	})
	sort.Slice(artifacts, func(i, j int) bool {
		if !artifacts[i].CreatedAt.Equal(artifacts[j].CreatedAt) {
			return artifacts[i].CreatedAt.After(artifacts[j].CreatedAt)
		}
		return artifacts[i].ID < artifacts[j].ID
	})
	if len(artifacts) > limit {
		artifacts = artifacts[:limit]
	}
	writeJSON(w, http.StatusOK, map[string]any{"artifacts": artifacts, "count": len(artifacts)})
}

func (s *Server) handleSandboxArtifactsList(w http.ResponseWriter, r *http.Request) {
	sandboxID := r.PathValue("sandboxID")
	if _, ok := s.authenticateSandbox(w, r, sandboxID); !ok {
		return
	}
	artifacts := []*core.Artifact{}
	_ = s.store.View(func(state *core.State) error {
		for _, artifact := range state.Artifacts {
			if artifact.Metadata["sandbox_id"] != sandboxID {
				continue
			}
			copy := cloneViaJSON(*artifact)
			artifacts = append(artifacts, &copy)
		}
		return nil
	})
	sort.Slice(artifacts, func(i, j int) bool {
		if !artifacts[i].CreatedAt.Equal(artifacts[j].CreatedAt) {
			return artifacts[i].CreatedAt.Before(artifacts[j].CreatedAt)
		}
		return artifacts[i].ID < artifacts[j].ID
	})
	writeJSON(w, http.StatusOK, map[string]any{"artifacts": artifacts, "count": len(artifacts)})
}

func (s *Server) handleSandboxArtifactDelete(w http.ResponseWriter, r *http.Request) {
	sandboxID := r.PathValue("sandboxID")
	sandbox, ok := s.authenticateSandbox(w, r, sandboxID)
	if !ok {
		return
	}
	if !sandbox.Permissions.DataWrite {
		writeAPIError(w, http.StatusForbidden, "data_write_not_allowed", "sandbox token does not grant data_write permission")
		return
	}
	s.artifactMu.Lock()
	defer s.artifactMu.Unlock()

	artifactID := r.PathValue("artifactID")
	found := false
	removeContent := false
	var deleted core.Artifact
	err := s.store.Update(func(state *core.State) error {
		artifact := state.Artifacts[artifactID]
		if artifact == nil || artifact.Metadata["sandbox_id"] != sandboxID {
			return fmt.Errorf("artifact is not associated with this sandbox")
		}
		found = true
		blockers := sandboxArtifactDeleteBlockers(state, artifactID, sandboxID)
		if len(blockers) > 0 {
			return fmt.Errorf("artifact is still needed by %s", strings.Join(blockers, ", "))
		}
		deleted = cloneViaJSON(*artifact)
		scrubSandboxArtifactReferences(state, artifactID, sandboxID)
		delete(state.Artifacts, artifactID)
		removeContent = artifactContentUnreferenced(state, artifact.StoragePath)
		state.Events = filterEvents(state.Events, "artifact", artifactID)
		addEvent(state, s.cfg.EventRetention, "artifact.deleted", "sandbox:"+sandboxID, "artifact", artifactID, "sandbox-scoped artifact deleted", map[string]string{"content_unreferenced": strconv.FormatBool(removeContent)})
		return nil
	})
	if err != nil {
		status := http.StatusConflict
		if !found {
			status = http.StatusNotFound
		}
		writeAPIError(w, status, "sandbox_artifact_delete_failed", err.Error())
		return
	}
	warnings, contentDeleted := s.removeArtifactContentIfUnreferenced(deleted, removeContent)
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "artifact_id": artifactID, "content_deleted": contentDeleted, "warnings": warnings})
}

func (s *Server) handleArtifactGet(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	artifactID := r.PathValue("artifactID")
	var artifact *core.Artifact
	_ = s.store.View(func(state *core.State) error {
		if current := state.Artifacts[artifactID]; current != nil {
			copy := cloneViaJSON(*current)
			artifact = &copy
		}
		return nil
	})
	if artifact == nil {
		writeAPIError(w, http.StatusNotFound, "artifact_not_found", "artifact not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"artifact": artifact})
}

func (s *Server) handleArtifactDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	s.artifactMu.Lock()
	defer s.artifactMu.Unlock()

	artifactID := r.PathValue("artifactID")
	force := strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("force")), "true")
	found := false
	removeContent := false
	var deleted core.Artifact
	references := []string{}
	err := s.store.Update(func(state *core.State) error {
		artifact := state.Artifacts[artifactID]
		if artifact == nil {
			return fmt.Errorf("artifact not found")
		}
		found = true
		references = artifactReferences(state, artifactID)
		if len(references) > 0 && !force {
			return fmt.Errorf("artifact is still referenced by %d objects; delete those objects first or use force=true", len(references))
		}
		deleted = cloneViaJSON(*artifact)
		if force {
			scrubArtifactReferences(state, artifactID)
		}
		delete(state.Artifacts, artifactID)
		removeContent = true
		for _, other := range state.Artifacts {
			if other.StoragePath == artifact.StoragePath {
				removeContent = false
				break
			}
		}
		state.Events = filterEvents(state.Events, "artifact", artifactID)
		addEvent(state, s.cfg.EventRetention, "artifact.deleted", actorFromRequest(r), "artifact", artifactID, "artifact metadata deleted", map[string]string{"content_unreferenced": strconv.FormatBool(removeContent)})
		return nil
	})
	if err != nil {
		status, code := http.StatusConflict, "artifact_delete_failed"
		if !found {
			status, code = http.StatusNotFound, "artifact_not_found"
		}
		writeAPIError(w, status, code, err.Error())
		return
	}
	warnings, contentDeleted := s.removeArtifactContentIfUnreferenced(deleted, removeContent)
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "artifact_id": artifactID, "content_deleted": contentDeleted, "references_removed": len(references), "warnings": warnings})
}

func artifactContentUnreferenced(state *core.State, storagePath string) bool {
	for _, other := range state.Artifacts {
		if other.StoragePath == storagePath {
			return false
		}
	}
	return true
}

func sandboxArtifactDeleteBlockers(state *core.State, artifactID, sandboxID string) []string {
	blockers := []string{}
	for _, task := range state.Tasks {
		for _, input := range task.Spec.Inputs {
			if input.ArtifactID == artifactID {
				blockers = append(blockers, "task:"+task.ID)
			}
		}
		if task.Result != nil && core.ContainsString(task.Result.OutputArtifactIDs, artifactID) {
			blockers = append(blockers, "task:"+task.ID)
		}
	}
	for _, execution := range state.Executions {
		if core.ContainsString(execution.OutputArtifactIDs, artifactID) {
			blockers = append(blockers, "execution:"+execution.ID)
		}
	}
	for _, operation := range state.SandboxOperations {
		references := (operation.File != nil && operation.File.ArtifactID == artifactID) || operation.ResultArtifactID == artifactID
		if references && (operation.SandboxID != sandboxID || !operation.Terminal()) {
			blockers = append(blockers, "sandbox_operation:"+operation.ID)
		}
	}
	sort.Strings(blockers)
	return blockers
}

func scrubSandboxArtifactReferences(state *core.State, artifactID, sandboxID string) {
	for _, operation := range state.SandboxOperations {
		if operation.SandboxID != sandboxID {
			continue
		}
		if operation.File != nil && operation.File.ArtifactID == artifactID {
			operation.File.ArtifactID = ""
		}
		if operation.ResultArtifactID == artifactID {
			operation.ResultArtifactID = ""
		}
	}
}

func sandboxArtifactHasExternalReferences(state *core.State, artifactID, sandboxID string) bool {
	for _, task := range state.Tasks {
		for _, input := range task.Spec.Inputs {
			if input.ArtifactID == artifactID {
				return true
			}
		}
		if task.Result != nil && core.ContainsString(task.Result.OutputArtifactIDs, artifactID) {
			return true
		}
	}
	for _, execution := range state.Executions {
		if core.ContainsString(execution.OutputArtifactIDs, artifactID) {
			return true
		}
	}
	for _, operation := range state.SandboxOperations {
		if operation.SandboxID == sandboxID {
			continue
		}
		if operation.File != nil && operation.File.ArtifactID == artifactID {
			return true
		}
		if operation.ResultArtifactID == artifactID {
			return true
		}
	}
	return false
}

func purgeSandboxArtifactsLocked(state *core.State, sandboxID string) []core.Artifact {
	removed := []core.Artifact{}
	for artifactID, artifact := range state.Artifacts {
		if artifact.Metadata["sandbox_id"] != sandboxID || sandboxArtifactHasExternalReferences(state, artifactID, sandboxID) {
			continue
		}
		removed = append(removed, cloneViaJSON(*artifact))
		scrubSandboxArtifactReferences(state, artifactID, sandboxID)
		delete(state.Artifacts, artifactID)
		state.Events = filterEvents(state.Events, "artifact", artifactID)
	}
	return removed
}

func (s *Server) removeArtifactContentIfUnreferenced(artifact core.Artifact, remove bool) ([]string, bool) {
	warnings := []string{}
	if !remove {
		return warnings, false
	}
	path, pathErr := artifactAbsolutePath(s.store.ArtifactsDir(), &artifact)
	if pathErr != nil {
		return append(warnings, "artifact content path was invalid; run artifact GC"), false
	}
	if removeErr := os.Remove(path); removeErr == nil || os.IsNotExist(removeErr) {
		return warnings, true
	} else {
		s.logger.Warn("artifact deletion left orphaned content", "artifact_id", artifact.ID, "error", removeErr)
		return append(warnings, "artifact metadata was deleted but content cleanup failed; run artifact GC"), false
	}
}

type artifactGCRequest struct {
	Delete bool `json:"delete,omitempty"`
}

func (s *Server) handleArtifactsGC(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	request := artifactGCRequest{}
	if r.ContentLength != 0 && !decodeJSON(w, r, s.cfg.MaxRequestBytes, &request) {
		return
	}
	s.artifactMu.Lock()
	defer s.artifactMu.Unlock()

	referenced := map[string]bool{}
	if err := s.store.View(func(state *core.State) error {
		for _, artifact := range state.Artifacts {
			path, err := artifactAbsolutePath(s.store.ArtifactsDir(), artifact)
			if err != nil {
				return err
			}
			referenced[path] = true
		}
		return nil
	}); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "artifact_gc_failed", err.Error())
		return
	}
	orphanCount := 0
	orphanBytes := int64(0)
	deletedCount := 0
	paths := []string{}
	err := filepath.WalkDir(s.store.ArtifactsDir(), func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		absolute, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		if referenced[absolute] {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		orphanCount++
		orphanBytes += info.Size()
		if len(paths) < 1000 {
			relative, _ := filepath.Rel(s.store.ArtifactsDir(), path)
			paths = append(paths, filepath.ToSlash(relative))
		}
		if request.Delete {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
			deletedCount++
		}
		return nil
	})
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "artifact_gc_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"dry_run": !request.Delete, "orphan_files": orphanCount, "orphan_bytes": orphanBytes, "deleted_files": deletedCount, "paths": paths, "paths_truncated": orphanCount > len(paths)})
}

func artifactReferences(state *core.State, artifactID string) []string {
	references := []string{}
	for _, task := range state.Tasks {
		for _, input := range task.Spec.Inputs {
			if input.ArtifactID == artifactID {
				references = append(references, "task:"+task.ID+":input")
			}
		}
		if task.Result != nil && core.ContainsString(task.Result.OutputArtifactIDs, artifactID) {
			references = append(references, "task:"+task.ID+":output")
		}
	}
	for _, execution := range state.Executions {
		if core.ContainsString(execution.OutputArtifactIDs, artifactID) {
			references = append(references, "execution:"+execution.ID+":output")
		}
	}
	for _, operation := range state.SandboxOperations {
		if operation.File != nil && operation.File.ArtifactID == artifactID {
			references = append(references, "sandbox_operation:"+operation.ID+":file")
		}
		if operation.ResultArtifactID == artifactID {
			references = append(references, "sandbox_operation:"+operation.ID+":result")
		}
	}
	sort.Strings(references)
	return references
}

func scrubArtifactReferences(state *core.State, artifactID string) {
	for _, task := range state.Tasks {
		inputs := make([]core.ArtifactInput, 0, len(task.Spec.Inputs))
		for _, input := range task.Spec.Inputs {
			if input.ArtifactID != artifactID {
				inputs = append(inputs, input)
			}
		}
		task.Spec.Inputs = inputs
		if task.Result != nil {
			task.Result.OutputArtifactIDs = withoutString(task.Result.OutputArtifactIDs, artifactID)
		}
	}
	for _, execution := range state.Executions {
		execution.OutputArtifactIDs = withoutString(execution.OutputArtifactIDs, artifactID)
	}
	for _, operation := range state.SandboxOperations {
		if operation.File != nil && operation.File.ArtifactID == artifactID {
			operation.File.ArtifactID = ""
		}
		if operation.ResultArtifactID == artifactID {
			operation.ResultArtifactID = ""
		}
	}
}

func withoutString(values []string, target string) []string {
	filtered := make([]string, 0, len(values))
	for _, value := range values {
		if value != target {
			filtered = append(filtered, value)
		}
	}
	return filtered
}

func (s *Server) handleArtifactDownload(w http.ResponseWriter, r *http.Request) {
	if !s.authenticateAdminOrNode(w, r) {
		return
	}
	artifactID := r.PathValue("artifactID")
	var artifact *core.Artifact
	_ = s.store.View(func(state *core.State) error {
		if current := state.Artifacts[artifactID]; current != nil {
			copy := cloneViaJSON(*current)
			artifact = &copy
		}
		return nil
	})
	if artifact == nil {
		writeAPIError(w, http.StatusNotFound, "artifact_not_found", "artifact not found")
		return
	}
	path, err := artifactAbsolutePath(s.store.ArtifactsDir(), artifact)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "artifact_path_invalid", err.Error())
		return
	}
	file, err := os.Open(path)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "artifact_content_missing", "artifact metadata exists but content is missing")
		return
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "artifact_stat_failed", err.Error())
		return
	}
	w.Header().Set("Content-Type", artifact.MediaType)
	w.Header().Set("Content-Length", strconv.FormatInt(stat.Size(), 10))
	w.Header().Set("Content-Disposition", contentDisposition(artifact.Name))
	w.Header().Set("ETag", fmt.Sprintf("\"sha256:%s\"", artifact.SHA256))
	w.Header().Set("Cache-Control", "private, no-store")
	http.ServeContent(w, r, artifact.Name, stat.ModTime(), file)
}

func mediaTypeFromName(name string) string {
	name = strings.ToLower(name)
	switch {
	case strings.HasSuffix(name, ".json"):
		return "application/json"
	case strings.HasSuffix(name, ".txt"), strings.HasSuffix(name, ".log"), strings.HasSuffix(name, ".md"):
		return "text/plain; charset=utf-8"
	case strings.HasSuffix(name, ".html"):
		return "text/html; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}
