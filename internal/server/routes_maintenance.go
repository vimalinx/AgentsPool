package server

import (
	"net/http"
	"sort"
	"time"

	"github.com/qiyuehuajin/agentspool/internal/core"
)

type maintenancePruneRequest struct {
	Execute bool `json:"execute"`
}

type maintenancePruneResult struct {
	DryRun                           bool     `json:"dry_run"`
	ResourceLeaseIDs                 []string `json:"resource_lease_ids"`
	SandboxIDs                       []string `json:"sandbox_ids"`
	SandboxVolumeIDs                 []string `json:"sandbox_volume_ids"`
	SandboxOperationIDs              []string `json:"sandbox_operation_ids"`
	ArtifactIDs                      []string `json:"artifact_ids"`
	ArtifactBytes                    int64    `json:"artifact_bytes"`
	DeviceObservationEvents          int      `json:"device_observation_events"`
	ResourceLeasesDeleted            int      `json:"resource_leases_deleted"`
	SandboxesDeleted                 int      `json:"sandboxes_deleted"`
	SandboxVolumesDeleted            int      `json:"sandbox_volumes_deleted"`
	SandboxOperationsDeleted         int      `json:"sandbox_operations_deleted"`
	ArtifactsDeleted                 int      `json:"artifacts_deleted"`
	ArtifactContentFilesDeleted      int      `json:"artifact_content_files_deleted"`
	RegistryObservationEventsDeleted int      `json:"registry_observation_events_deleted"`
	Warnings                         []string `json:"warnings"`
}

func artifactReferencedOutsideOperations(state *core.State, artifactID string, deletingOperations map[string]bool) bool {
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
		if deletingOperations[operation.ID] {
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

func terminalSandboxHistory(state *core.State) maintenancePruneResult {
	result := maintenancePruneResult{
		DryRun: true, Warnings: []string{}, ResourceLeaseIDs: []string{}, SandboxIDs: []string{},
		SandboxVolumeIDs: []string{}, SandboxOperationIDs: []string{}, ArtifactIDs: []string{},
	}
	deletableSandboxes := map[string]bool{}
	for sandboxID, sandbox := range state.Sandboxes {
		if sandbox.Status != core.SandboxDestroyed {
			continue
		}
		allTerminal := true
		for _, operation := range state.SandboxOperations {
			if operation.SandboxID == sandboxID && !operation.Terminal() {
				allTerminal = false
				break
			}
		}
		if allTerminal {
			deletableSandboxes[sandboxID] = true
			result.SandboxIDs = append(result.SandboxIDs, sandboxID)
		}
	}

	deletingOperations := map[string]bool{}
	for operationID, operation := range state.SandboxOperations {
		if !operation.Terminal() {
			continue
		}
		sandbox := state.Sandboxes[operation.SandboxID]
		// Terminal results are explicitly compactable while their persistent
		// sandbox stays alive. The active service operation is non-terminal and
		// remains available without imposing an arbitrary age or count limit.
		if sandbox == nil || sandbox.Status != core.SandboxDestroyed || deletableSandboxes[operation.SandboxID] {
			deletingOperations[operationID] = true
			result.SandboxOperationIDs = append(result.SandboxOperationIDs, operationID)
		}
	}
	for volumeID, volume := range state.SandboxVolumes {
		if deletableSandboxes[volume.SandboxID] {
			result.SandboxVolumeIDs = append(result.SandboxVolumeIDs, volumeID)
		}
	}
	for leaseID, lease := range state.ResourceLeases {
		if lease.Status == core.ResourceLeaseActive {
			continue
		}
		if lease.SandboxID == "" || deletableSandboxes[lease.SandboxID] {
			result.ResourceLeaseIDs = append(result.ResourceLeaseIDs, leaseID)
		}
	}

	for artifactID, artifact := range state.Artifacts {
		sandboxID := artifact.Metadata["sandbox_id"]
		if sandboxID == "" {
			continue
		}
		_, sandboxExists := state.Sandboxes[sandboxID]
		candidate := !sandboxExists || deletableSandboxes[sandboxID]
		if operationID := artifact.Metadata["sandbox_operation_id"]; deletingOperations[operationID] {
			candidate = true
		}
		if !candidate {
			for operationID := range deletingOperations {
				operation := state.SandboxOperations[operationID]
				if operation == nil || operation.SandboxID != sandboxID {
					continue
				}
				if (operation.File != nil && operation.File.ArtifactID == artifactID) || operation.ResultArtifactID == artifactID {
					candidate = true
					break
				}
			}
		}
		if !candidate && len(artifactReferences(state, artifactID)) == 0 {
			candidate = true
		}
		if candidate && !artifactReferencedOutsideOperations(state, artifactID, deletingOperations) {
			result.ArtifactIDs = append(result.ArtifactIDs, artifactID)
			result.ArtifactBytes += artifact.Size
		}
	}
	for _, event := range state.RegistryEvents {
		if event.Type == "device.observed" {
			result.DeviceObservationEvents++
		}
	}
	for _, values := range [][]string{result.ResourceLeaseIDs, result.SandboxIDs, result.SandboxVolumeIDs, result.SandboxOperationIDs, result.ArtifactIDs} {
		sort.Strings(values)
	}
	return result
}

func (s *Server) handleMaintenancePrune(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	request := maintenancePruneRequest{}
	if !decodeJSON(w, r, s.cfg.MaxRequestBytes, &request) {
		return
	}
	var result maintenancePruneResult
	if !request.Execute {
		_ = s.store.View(func(state *core.State) error {
			result = terminalSandboxHistory(state)
			return nil
		})
		writeJSON(w, http.StatusOK, result)
		return
	}

	s.artifactMu.Lock()
	defer s.artifactMu.Unlock()
	now := time.Now().UTC()
	removedArtifacts := []core.Artifact{}
	removeContent := map[string]bool{}
	err := s.store.Update(func(state *core.State) error {
		result = terminalSandboxHistory(state)
		result.DryRun = false
		for _, operationID := range result.SandboxOperationIDs {
			delete(state.SandboxOperations, operationID)
		}
		for _, volumeID := range result.SandboxVolumeIDs {
			delete(state.SandboxVolumes, volumeID)
		}
		for _, sandboxID := range result.SandboxIDs {
			delete(state.Sandboxes, sandboxID)
		}
		for _, leaseID := range result.ResourceLeaseIDs {
			delete(state.ResourceLeases, leaseID)
		}
		for _, artifactID := range result.ArtifactIDs {
			artifact := state.Artifacts[artifactID]
			if artifact == nil {
				continue
			}
			removedArtifacts = append(removedArtifacts, cloneViaJSON(*artifact))
			delete(state.Artifacts, artifactID)
			state.Events = filterEvents(state.Events, "artifact", artifactID)
		}
		for _, artifact := range removedArtifacts {
			removeContent[artifact.StoragePath] = artifactContentUnreferenced(state, artifact.StoragePath)
		}
		filteredRegistryEvents := make([]core.RegistryEvent, 0, len(state.RegistryEvents))
		for _, event := range state.RegistryEvents {
			if event.Type != "device.observed" {
				filteredRegistryEvents = append(filteredRegistryEvents, event)
			}
		}
		state.RegistryEvents = filteredRegistryEvents
		result.ResourceLeasesDeleted = len(result.ResourceLeaseIDs)
		result.SandboxesDeleted = len(result.SandboxIDs)
		result.SandboxVolumesDeleted = len(result.SandboxVolumeIDs)
		result.SandboxOperationsDeleted = len(result.SandboxOperationIDs)
		result.ArtifactsDeleted = len(removedArtifacts)
		result.RegistryObservationEventsDeleted = result.DeviceObservationEvents
		addEvent(state, s.cfg.EventRetention, "maintenance.pruned", actorFromRequest(r), "maintenance", "terminal-sandbox-history", "pruned terminal operations, sandbox artifacts, and obsolete observation events", map[string]string{"at": now.Format(time.RFC3339)})
		bumpRegistry(state, s.cfg.EventRetention, "maintenance.pruned", "maintenance", "terminal-sandbox-history", nil)
		return nil
	})
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "maintenance_prune_failed", err.Error())
		return
	}
	for _, operationID := range result.SandboxOperationIDs {
		if err := s.operationLogs.Delete(operationID); err != nil {
			result.Warnings = append(result.Warnings, "remove operation logs: "+err.Error())
		}
	}
	removedPaths := map[string]bool{}
	for _, artifact := range removedArtifacts {
		if !removeContent[artifact.StoragePath] || removedPaths[artifact.StoragePath] {
			continue
		}
		removedPaths[artifact.StoragePath] = true
		warnings, deleted := s.removeArtifactContentIfUnreferenced(artifact, true)
		result.Warnings = append(result.Warnings, warnings...)
		if deleted {
			result.ArtifactContentFilesDeleted++
		}
	}
	s.notifyRegistryChanged()
	writeJSON(w, http.StatusOK, result)
}
