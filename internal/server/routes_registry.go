package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/vimalinx/AgentsPool/internal/core"
)

type deviceSnapshot struct {
	ID                 string                   `json:"id"`
	Name               string                   `json:"name"`
	Status             string                   `json:"status"`
	Capabilities       core.NodeCapabilities    `json:"capabilities"`
	Descriptions       []core.DeviceDescription `json:"descriptions,omitempty"`
	Observed           core.NodeResources       `json:"observed"`
	Reserved           core.ResourceRequest     `json:"reserved"`
	Available          core.ResourceRequest     `json:"available"`
	ActiveSandboxCount int                      `json:"active_sandbox_count"`
	LastSeenAt         time.Time                `json:"last_seen_at"`
}

type resourceLeaseAcquireRequest struct {
	NodeID           string               `json:"node_id"`
	Owner            string               `json:"owner,omitempty"`
	ExpectedRevision int64                `json:"expected_revision"`
	Resources        core.ResourceRequest `json:"resources"`
	TTLSeconds       int                  `json:"ttl_seconds,omitempty"`
}

type resourceLeaseResizeRequest struct {
	ExpectedRevision int64                `json:"expected_revision"`
	Resources        core.ResourceRequest `json:"resources"`
}

type resourceLeaseRenewRequest struct {
	TTLSeconds *int `json:"ttl_seconds,omitempty"`
}

type publishedService struct {
	SandboxID    string                        `json:"sandbox_id"`
	SandboxName  string                        `json:"sandbox_name"`
	NodeID       string                        `json:"node_id"`
	OperationID  string                        `json:"operation_id"`
	Status       string                        `json:"status"`
	StartedAt    *time.Time                    `json:"started_at,omitempty"`
	Endpoints    []core.SandboxEndpoint        `json:"endpoints"`
	Health       string                        `json:"health"`
	HealthAt     *time.Time                    `json:"health_checked_at,omitempty"`
	HealthChecks []publishedServiceHealthCheck `json:"health_checks,omitempty"`
}

type publishedServiceHealthCheck struct {
	EndpointName string `json:"endpoint_name"`
	URL          string `json:"url"`
	Healthy      bool   `json:"healthy"`
	HTTPStatus   int    `json:"http_status,omitempty"`
	Error        string `json:"error,omitempty"`
}

func probePublishedService(parent context.Context, service *publishedService) {
	checks := []publishedServiceHealthCheck{}
	for _, endpoint := range service.Endpoints {
		if endpoint.HealthURL == "" {
			continue
		}
		check := publishedServiceHealthCheck{EndpointName: endpoint.Name, URL: endpoint.HealthURL}
		ctx, cancel := context.WithTimeout(parent, 3*time.Second)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.HealthURL, nil)
		if err == nil {
			resp, requestErr := http.DefaultClient.Do(req)
			if requestErr != nil {
				err = requestErr
			} else {
				check.HTTPStatus = resp.StatusCode
				_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
				_ = resp.Body.Close()
				check.Healthy = resp.StatusCode >= 200 && resp.StatusCode < 400
				if !check.Healthy {
					check.Error = resp.Status
				}
			}
		}
		cancel()
		if err != nil {
			check.Error = err.Error()
		}
		checks = append(checks, check)
	}
	service.HealthChecks = checks
	if len(checks) == 0 {
		service.Health = "unknown"
		return
	}
	now := time.Now().UTC()
	service.HealthAt = &now
	service.Health = "healthy"
	for _, check := range checks {
		if !check.Healthy {
			service.Health = "unhealthy"
			break
		}
	}
}

func activeResourceReservation(state *core.State, nodeID string, now time.Time, excludeLeaseID string) core.ResourceRequest {
	var reserved core.ResourceRequest
	for _, lease := range state.ResourceLeases {
		if lease.ID == excludeLeaseID || lease.NodeID != nodeID {
			continue
		}
		resources := lease.Resources
		sandbox := state.Sandboxes[lease.SandboxID]
		persistent := sandbox != nil && sandbox.Status != core.SandboxDestroyed
		executing := false
		for _, operation := range state.SandboxOperations {
			if !persistent || operation.SandboxID != sandbox.ID || operation.Terminal() {
				continue
			}
			if operation.Status != core.SandboxOperationQueued {
				executing = true
			}
			if operation.Resources != nil {
				resources.CPUMillis = max64(resources.CPUMillis, operation.Resources.CPUMillis)
				resources.MemoryMB = max64(resources.MemoryMB, operation.Resources.MemoryMB)
				resources.DiskMB = max64(resources.DiskMB, operation.Resources.DiskMB)
			}
		}
		// Expiry is not proof that a remote process stopped. Keep its compute
		// reservation until the Worker acknowledges termination. Persistent disk
		// remains reserved even while stopped or awaiting a replacement lease.
		if (lease.ActiveAt(now) && !lease.ComputeReleased) || executing {
			reserved.CPUMillis += resources.CPUMillis
			reserved.MemoryMB += resources.MemoryMB
		}
		if lease.ActiveAt(now) || persistent {
			reserved.DiskMB += resources.DiskMB
		}
	}
	return reserved
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func deviceSnapshotLocked(state *core.State, node *core.Node, now time.Time) deviceSnapshot {
	reserved := activeResourceReservation(state, node.ID, now, "")
	cpuAvailable := max64(int64(node.Capabilities.CPUThreads)*1000-reserved.CPUMillis, 0)
	memoryAvailable := max64(min64(node.Capabilities.MemoryTotalMB-reserved.MemoryMB, node.Resources.MemoryAvailableMB), 0)
	diskAvailable := max64(min64(node.Capabilities.DiskTotalMB-reserved.DiskMB, node.Resources.DiskAvailableMB), 0)
	activeSandboxes := 0
	for _, sandbox := range state.Sandboxes {
		if sandbox.NodeID == node.ID && sandbox.Status != core.SandboxDestroyed {
			activeSandboxes++
		}
	}
	return deviceSnapshot{
		ID: node.ID, Name: node.Name, Status: node.Status, Capabilities: cloneViaJSON(node.Capabilities),
		Descriptions: sortedDeviceDescriptions(node.Descriptions),
		Observed:     cloneViaJSON(node.Resources), Reserved: reserved,
		Available:          core.ResourceRequest{CPUMillis: cpuAvailable, MemoryMB: memoryAvailable, DiskMB: diskAvailable},
		ActiveSandboxCount: activeSandboxes, LastSeenAt: node.LastSeenAt,
	}
}

func registrySnapshot(state *core.State, now time.Time) []deviceSnapshot {
	devices := make([]deviceSnapshot, 0, len(state.Nodes))
	for _, node := range state.Nodes {
		devices = append(devices, deviceSnapshotLocked(state, node, now))
	}
	sort.Slice(devices, func(i, j int) bool {
		if devices[i].Name != devices[j].Name {
			return devices[i].Name < devices[j].Name
		}
		return devices[i].ID < devices[j].ID
	})
	return devices
}

func (s *Server) handleDevicesList(w http.ResponseWriter, r *http.Request) {
	if !s.requireAllocator(w, r) {
		return
	}
	state, err := s.store.Snapshot()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "snapshot_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revision": state.RegistryRevision, "devices": registrySnapshot(&state, time.Now().UTC())})
}

func (s *Server) handleServicesList(w http.ResponseWriter, r *http.Request) {
	if !s.requireAllocator(w, r) {
		return
	}
	services := []publishedService{}
	_ = s.store.View(func(state *core.State) error {
		for _, sandbox := range state.Sandboxes {
			if sandbox.ServiceOperationID == "" || len(sandbox.PublishedEndpoints) == 0 {
				continue
			}
			operation := state.SandboxOperations[sandbox.ServiceOperationID]
			if operation == nil || operation.Terminal() {
				continue
			}
			services = append(services, publishedService{
				SandboxID: sandbox.ID, SandboxName: sandbox.Name, NodeID: sandbox.NodeID,
				OperationID: operation.ID, Status: operation.Status, StartedAt: operation.StartedAt,
				Endpoints: cloneViaJSON(sandbox.PublishedEndpoints),
			})
		}
		return nil
	})
	sort.Slice(services, func(i, j int) bool {
		if services[i].NodeID != services[j].NodeID {
			return services[i].NodeID < services[j].NodeID
		}
		return services[i].SandboxID < services[j].SandboxID
	})
	for index := range services {
		probePublishedService(r.Context(), &services[index])
	}
	writeJSON(w, http.StatusOK, map[string]any{"services": services, "count": len(services)})
}

func (s *Server) handleDeviceGet(w http.ResponseWriter, r *http.Request) {
	if !s.requireAllocator(w, r) {
		return
	}
	state, err := s.store.Snapshot()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "snapshot_failed", err.Error())
		return
	}
	node := state.Nodes[r.PathValue("nodeID")]
	if node == nil {
		writeAPIError(w, http.StatusNotFound, "device_not_found", "device not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revision": state.RegistryRevision, "device": deviceSnapshotLocked(&state, node, time.Now().UTC())})
}

func (s *Server) handleDevicesWatch(w http.ResponseWriter, r *http.Request) {
	if !s.requireAllocator(w, r) {
		return
	}
	since, err := strconv.ParseInt(strings.TrimSpace(r.URL.Query().Get("since_revision")), 10, 64)
	if err != nil || since < 0 {
		writeAPIError(w, http.StatusBadRequest, "invalid_revision", "since_revision must be a non-negative integer")
		return
	}
	waitSeconds := 25
	if raw := strings.TrimSpace(r.URL.Query().Get("wait_seconds")); raw != "" {
		value, parseErr := strconv.Atoi(raw)
		if parseErr != nil || value < 0 || value > 30 {
			writeAPIError(w, http.StatusBadRequest, "invalid_wait", "wait_seconds must be between 0 and 30")
			return
		}
		waitSeconds = value
	}
	deadline := time.NewTimer(time.Duration(waitSeconds) * time.Second)
	defer deadline.Stop()
	for {
		changed := s.registryWaitChannel()
		state, snapshotErr := s.store.Snapshot()
		if snapshotErr != nil {
			writeAPIError(w, http.StatusInternalServerError, "snapshot_failed", snapshotErr.Error())
			return
		}
		if state.RegistryRevision > since || waitSeconds == 0 {
			events := make([]core.RegistryEvent, 0)
			for _, event := range state.RegistryEvents {
				if event.Revision > since {
					events = append(events, event)
				}
			}
			resetRequired := len(state.RegistryEvents) > 0 && since+1 < state.RegistryEvents[0].Revision
			writeJSON(w, http.StatusOK, map[string]any{
				"revision": state.RegistryRevision, "changed": state.RegistryRevision > since,
				"reset_required": resetRequired, "events": events, "devices": registrySnapshot(&state, time.Now().UTC()),
			})
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-deadline.C:
			writeJSON(w, http.StatusOK, map[string]any{"revision": state.RegistryRevision, "changed": false, "events": []core.RegistryEvent{}, "devices": registrySnapshot(&state, time.Now().UTC())})
			return
		case <-changed:
		}
	}
}

func validateResourceRequest(resources core.ResourceRequest) error {
	if resources.CPUMillis < 10 {
		return fmt.Errorf("cpu_millis must be at least 10")
	}
	if resources.MemoryMB < 16 {
		return fmt.Errorf("memory_mb must be at least 16")
	}
	if resources.DiskMB < 32 {
		return fmt.Errorf("disk_mb must be at least 32")
	}
	return nil
}

func publicResourceLease(lease *core.ResourceLease) core.ResourceLease {
	copy := cloneViaJSON(*lease)
	copy.TokenHash = ""
	return copy
}

func validateAdmission(state *core.State, node *core.Node, request core.ResourceRequest, now time.Time, excludeLeaseID string) error {
	if node.Status != core.NodeOnline {
		return fmt.Errorf("node is not online")
	}
	reserved := activeResourceReservation(state, node.ID, now, excludeLeaseID)
	available := core.ResourceRequest{
		CPUMillis: max64(int64(node.Capabilities.CPUThreads)*1000-reserved.CPUMillis, 0),
		MemoryMB:  max64(min64(node.Capabilities.MemoryTotalMB-reserved.MemoryMB, node.Resources.MemoryAvailableMB), 0),
		DiskMB:    max64(min64(node.Capabilities.DiskTotalMB-reserved.DiskMB, node.Resources.DiskAvailableMB), 0),
	}
	// An existing persistent image already consumes the disk reported by the
	// Worker. Restart needs no second copy; resize needs only the growth delta.
	physicalDiskNeeded := request.DiskMB
	if lease := state.ResourceLeases[excludeLeaseID]; lease != nil {
		if sandbox := state.Sandboxes[lease.SandboxID]; sandbox != nil && sandbox.Status != core.SandboxDestroyed {
			physicalDiskNeeded = max64(request.DiskMB-lease.Resources.DiskMB, 0)
		}
	}
	diskBudget := max64(node.Capabilities.DiskTotalMB-reserved.DiskMB, 0)
	if request.CPUMillis > available.CPUMillis || request.MemoryMB > available.MemoryMB || request.DiskMB > diskBudget || physicalDiskNeeded > node.Resources.DiskAvailableMB {
		return fmt.Errorf("insufficient resources: requested cpu=%dm memory=%dMB disk=%dMB; available cpu=%dm memory=%dMB disk=%dMB", request.CPUMillis, request.MemoryMB, request.DiskMB, available.CPUMillis, available.MemoryMB, available.DiskMB)
	}
	return nil
}

func (s *Server) handleResourceLeaseAcquire(w http.ResponseWriter, r *http.Request) {
	if !s.requireAllocator(w, r) {
		return
	}
	var request resourceLeaseAcquireRequest
	if !decodeJSON(w, r, s.cfg.MaxRequestBytes, &request) {
		return
	}
	request.NodeID = strings.TrimSpace(request.NodeID)
	request.Owner = strings.TrimSpace(request.Owner)
	if request.NodeID == "" {
		writeAPIError(w, http.StatusBadRequest, "invalid_node_id", "node_id is required")
		return
	}
	if len(request.Owner) > 200 {
		writeAPIError(w, http.StatusBadRequest, "invalid_owner", "owner must not exceed 200 characters")
		return
	}
	if err := validateResourceRequest(request.Resources); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_resources", err.Error())
		return
	}
	if request.TTLSeconds < 0 {
		writeAPIError(w, http.StatusBadRequest, "invalid_ttl", "ttl_seconds must be zero or greater")
		return
	}
	secret, err := core.NewSecret(32)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "token_generation_failed", err.Error())
		return
	}
	now := time.Now().UTC()
	var lease *core.ResourceLease
	var leaseToken string
	var conflictCode string
	err = s.store.Update(func(state *core.State) error {
		if request.ExpectedRevision != state.RegistryRevision {
			conflictCode = "stale_registry_revision"
			return fmt.Errorf("expected revision %d, current revision is %d", request.ExpectedRevision, state.RegistryRevision)
		}
		node := state.Nodes[request.NodeID]
		if node == nil {
			conflictCode = "node_not_found"
			return fmt.Errorf("node not found")
		}
		if admissionErr := validateAdmission(state, node, request.Resources, now, ""); admissionErr != nil {
			conflictCode = "admission_denied"
			return admissionErr
		}
		leaseID, idErr := core.NewID("rlease")
		if idErr != nil {
			return idErr
		}
		leaseToken = leaseID + "." + secret
		lease = &core.ResourceLease{
			ID: leaseID, NodeID: node.ID, Owner: request.Owner, TokenHash: core.HashSecret(leaseToken),
			Status: core.ResourceLeaseActive, Resources: request.Resources, TTLSeconds: request.TTLSeconds,
			CreatedAt: now, UpdatedAt: now, LastRenewedAt: now,
		}
		if request.TTLSeconds > 0 {
			expiresAt := now.Add(core.SecondsDuration(request.TTLSeconds))
			lease.ExpiresAt = &expiresAt
		}
		state.ResourceLeases[leaseID] = lease
		lease.RegistryRevision = bumpRegistry(state, s.cfg.EventRetention, "resource_lease.acquired", "resource_lease", leaseID, map[string]string{"node_id": node.ID})
		return nil
	})
	if err != nil {
		if conflictCode != "" {
			writeAPIError(w, http.StatusConflict, conflictCode, err.Error())
		} else {
			writeAPIError(w, http.StatusInternalServerError, "resource_lease_failed", err.Error())
		}
		return
	}
	s.notifyRegistryChanged()
	public := publicResourceLease(lease)
	writeJSON(w, http.StatusCreated, map[string]any{"lease": public, "lease_token": leaseToken, "revision": public.RegistryRevision})
}

func (s *Server) handleResourceLeasesList(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	leases := []core.ResourceLease{}
	_ = s.store.View(func(state *core.State) error {
		for _, lease := range state.ResourceLeases {
			leases = append(leases, publicResourceLease(lease))
		}
		return nil
	})
	sort.Slice(leases, func(i, j int) bool { return leases[i].CreatedAt.After(leases[j].CreatedAt) })
	writeJSON(w, http.StatusOK, map[string]any{"leases": leases, "count": len(leases)})
}

func (s *Server) handleResourceLeaseGet(w http.ResponseWriter, r *http.Request) {
	lease, ok := s.authenticateResourceLease(w, r, r.PathValue("leaseID"))
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"lease": publicResourceLease(lease)})
}

func (s *Server) handleResourceLeaseRenew(w http.ResponseWriter, r *http.Request) {
	leaseID := r.PathValue("leaseID")
	if _, ok := s.authenticateResourceLease(w, r, leaseID); !ok {
		return
	}
	var request resourceLeaseRenewRequest
	if !decodeJSON(w, r, s.cfg.MaxRequestBytes, &request) {
		return
	}
	now := time.Now().UTC()
	var response core.ResourceLease
	err := s.store.Update(func(state *core.State) error {
		lease := state.ResourceLeases[leaseID]
		if !lease.ActiveAt(now) {
			return fmt.Errorf("resource lease is not active")
		}
		if request.TTLSeconds != nil {
			if *request.TTLSeconds < 0 {
				return fmt.Errorf("ttl_seconds must be zero or greater")
			}
			lease.TTLSeconds = *request.TTLSeconds
		}
		lease.ExpiresAt = nil
		if lease.TTLSeconds > 0 {
			expiresAt := now.Add(core.SecondsDuration(lease.TTLSeconds))
			lease.ExpiresAt = &expiresAt
		}
		lease.LastRenewedAt = now
		lease.UpdatedAt = now
		lease.RegistryRevision = bumpRegistry(state, s.cfg.EventRetention, "resource_lease.renewed", "resource_lease", leaseID, nil)
		response = publicResourceLease(lease)
		return nil
	})
	if err != nil {
		writeAPIError(w, http.StatusConflict, "resource_lease_renew_failed", err.Error())
		return
	}
	s.notifyRegistryChanged()
	writeJSON(w, http.StatusOK, map[string]any{"lease": response, "revision": response.RegistryRevision})
}

func (s *Server) handleResourceLeaseResize(w http.ResponseWriter, r *http.Request) {
	leaseID := r.PathValue("leaseID")
	if _, ok := s.authenticateResourceLease(w, r, leaseID); !ok {
		return
	}
	var request resourceLeaseResizeRequest
	if !decodeJSON(w, r, s.cfg.MaxRequestBytes, &request) {
		return
	}
	if err := validateResourceRequest(request.Resources); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_resources", err.Error())
		return
	}
	now := time.Now().UTC()
	var response core.ResourceLease
	err := s.store.Update(func(state *core.State) error {
		if request.ExpectedRevision != state.RegistryRevision {
			return fmt.Errorf("expected revision %d, current revision is %d", request.ExpectedRevision, state.RegistryRevision)
		}
		lease := state.ResourceLeases[leaseID]
		if !lease.ActiveAt(now) {
			return fmt.Errorf("resource lease is not active")
		}
		if lease.SandboxID != "" {
			return fmt.Errorf("resize an attached lease through its sandbox endpoint")
		}
		node := state.Nodes[lease.NodeID]
		if node == nil {
			return fmt.Errorf("node not found")
		}
		if admissionErr := validateAdmission(state, node, request.Resources, now, leaseID); admissionErr != nil {
			return admissionErr
		}
		lease.Resources = request.Resources
		lease.UpdatedAt = now
		lease.RegistryRevision = bumpRegistry(state, s.cfg.EventRetention, "resource_lease.resized", "resource_lease", leaseID, nil)
		response = publicResourceLease(lease)
		return nil
	})
	if err != nil {
		writeAPIError(w, http.StatusConflict, "resource_lease_resize_failed", err.Error())
		return
	}
	s.notifyRegistryChanged()
	writeJSON(w, http.StatusOK, map[string]any{"lease": response, "revision": response.RegistryRevision})
}

func (s *Server) handleResourceLeaseRelease(w http.ResponseWriter, r *http.Request) {
	leaseID := r.PathValue("leaseID")
	if _, ok := s.authenticateResourceLease(w, r, leaseID); !ok {
		return
	}
	now := time.Now().UTC()
	var response core.ResourceLease
	err := s.store.Update(func(state *core.State) error {
		lease := state.ResourceLeases[leaseID]
		if lease == nil {
			return fmt.Errorf("resource lease not found")
		}
		if lease.SandboxID != "" {
			if sandbox := state.Sandboxes[lease.SandboxID]; sandbox != nil && sandbox.Status != core.SandboxDestroyed {
				return fmt.Errorf("destroy the attached sandbox before releasing its resource lease")
			}
		}
		if lease.Status == core.ResourceLeaseActive {
			lease.Status = core.ResourceLeaseReleased
			lease.ReleasedAt = &now
			lease.UpdatedAt = now
			lease.RegistryRevision = bumpRegistry(state, s.cfg.EventRetention, "resource_lease.released", "resource_lease", leaseID, nil)
		}
		response = publicResourceLease(lease)
		return nil
	})
	if err != nil {
		writeAPIError(w, http.StatusConflict, "resource_lease_release_failed", err.Error())
		return
	}
	s.notifyRegistryChanged()
	writeJSON(w, http.StatusOK, map[string]any{"lease": response, "revision": response.RegistryRevision})
}
