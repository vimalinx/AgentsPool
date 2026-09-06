package server

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/vimalinx/AgentsPool/internal/core"
)

type deviceDescriptionRequest struct {
	ExpectedRevision   int64    `json:"expected_revision"`
	Source             string   `json:"source"`
	Author             string   `json:"author"`
	Description        string   `json:"description,omitempty"`
	Availability       string   `json:"availability,omitempty"`
	AvailabilityNotes  string   `json:"availability_notes,omitempty"`
	ServicePolicy      string   `json:"service_policy,omitempty"`
	PreferredWorkloads []string `json:"preferred_workloads,omitempty"`
	AvoidWorkloads     []string `json:"avoid_workloads,omitempty"`
	Constraints        []string `json:"constraints,omitempty"`
}

type deviceDescriptionRemoveRequest struct {
	ExpectedRevision int64  `json:"expected_revision"`
	Source           string `json:"source"`
	Author           string `json:"author"`
}

func sortedDeviceDescriptions(values []core.DeviceDescription) []core.DeviceDescription {
	out := cloneViaJSON(values)
	sort.Slice(out, func(i, j int) bool {
		priority := func(source string) int {
			if source == core.DeviceDescriptionSourceHuman {
				return 0
			}
			return 1
		}
		if priority(out[i].Source) != priority(out[j].Source) {
			return priority(out[i].Source) < priority(out[j].Source)
		}
		if out[i].Author != out[j].Author {
			return out[i].Author < out[j].Author
		}
		return out[i].UpdatedAt.Before(out[j].UpdatedAt)
	})
	return out
}

func validateDeviceDescriptionIdentity(source, author string) (string, string, error) {
	source = strings.TrimSpace(source)
	author = strings.TrimSpace(author)
	if source != core.DeviceDescriptionSourceHuman && source != core.DeviceDescriptionSourceAgent {
		return "", "", fmt.Errorf("source must be human or agent")
	}
	if author == "" || utf8.RuneCountInString(author) > 100 || strings.ContainsRune(author, '\x00') {
		return "", "", fmt.Errorf("author is required and must not exceed 100 characters")
	}
	return source, author, nil
}

func normalizeDescriptionList(name string, values []string, itemLimit int) ([]string, error) {
	if len(values) > 32 {
		return nil, fmt.Errorf("%s must not contain more than 32 entries", name)
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || utf8.RuneCountInString(value) > itemLimit || strings.ContainsRune(value, '\x00') {
			return nil, fmt.Errorf("%s entries must be non-empty and not exceed %d characters", name, itemLimit)
		}
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out, nil
}

func normalizeDeviceDescription(request deviceDescriptionRequest, now time.Time) (core.DeviceDescription, error) {
	source, author, err := validateDeviceDescriptionIdentity(request.Source, request.Author)
	if err != nil {
		return core.DeviceDescription{}, err
	}
	description := strings.TrimSpace(request.Description)
	availabilityNotes := strings.TrimSpace(request.AvailabilityNotes)
	if utf8.RuneCountInString(description) > 2000 || strings.ContainsRune(description, '\x00') {
		return core.DeviceDescription{}, fmt.Errorf("description must not exceed 2000 characters")
	}
	if utf8.RuneCountInString(availabilityNotes) > 1000 || strings.ContainsRune(availabilityNotes, '\x00') {
		return core.DeviceDescription{}, fmt.Errorf("availability_notes must not exceed 1000 characters")
	}
	availability := strings.TrimSpace(request.Availability)
	if availability == "" {
		availability = core.DeviceAvailabilityUnknown
	}
	switch availability {
	case core.DeviceAvailabilityUnknown, core.DeviceAvailabilityAlwaysOn, core.DeviceAvailabilityScheduled, core.DeviceAvailabilityOnDemand:
	default:
		return core.DeviceDescription{}, fmt.Errorf("availability must be unknown, always_on, scheduled, or on_demand")
	}
	servicePolicy := strings.TrimSpace(request.ServicePolicy)
	if servicePolicy == "" {
		servicePolicy = core.DeviceServicePolicyUnknown
	}
	switch servicePolicy {
	case core.DeviceServicePolicyUnknown, core.DeviceServicePolicyPreferred, core.DeviceServicePolicyAllowed, core.DeviceServicePolicyAvoid, core.DeviceServicePolicyForbidden:
	default:
		return core.DeviceDescription{}, fmt.Errorf("service_policy must be unknown, preferred, allowed, avoid, or forbidden")
	}
	preferred, err := normalizeDescriptionList("preferred_workloads", request.PreferredWorkloads, 100)
	if err != nil {
		return core.DeviceDescription{}, err
	}
	avoid, err := normalizeDescriptionList("avoid_workloads", request.AvoidWorkloads, 100)
	if err != nil {
		return core.DeviceDescription{}, err
	}
	constraints, err := normalizeDescriptionList("constraints", request.Constraints, 500)
	if err != nil {
		return core.DeviceDescription{}, err
	}
	if description == "" && availabilityNotes == "" && availability == core.DeviceAvailabilityUnknown && servicePolicy == core.DeviceServicePolicyUnknown && len(preferred) == 0 && len(avoid) == 0 && len(constraints) == 0 {
		return core.DeviceDescription{}, fmt.Errorf("description must contain at least one useful field")
	}
	return core.DeviceDescription{
		Source: source, Author: author, Description: description,
		Availability: availability, AvailabilityNotes: availabilityNotes, ServicePolicy: servicePolicy,
		PreferredWorkloads: preferred, AvoidWorkloads: avoid, Constraints: constraints,
		BasisRevision: request.ExpectedRevision, UpdatedAt: now,
	}, nil
}

func (s *Server) handleDeviceDescriptionUpsert(w http.ResponseWriter, r *http.Request) {
	if !s.requireAllocator(w, r) {
		return
	}
	var request deviceDescriptionRequest
	if !decodeJSON(w, r, s.cfg.MaxRequestBytes, &request) {
		return
	}
	now := time.Now().UTC()
	description, err := normalizeDeviceDescription(request, now)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_device_description", err.Error())
		return
	}
	if description.Source == core.DeviceDescriptionSourceHuman && !isAdminRequest(r) {
		writeAPIError(w, http.StatusForbidden, "admin_required", "only an admin may write a human device description")
		return
	}
	nodeID := r.PathValue("nodeID")
	var snapshot deviceSnapshot
	var revision int64
	created := false
	conflictCode := ""
	err = s.store.Update(func(state *core.State) error {
		if request.ExpectedRevision != state.RegistryRevision {
			conflictCode = "stale_registry_revision"
			return fmt.Errorf("expected revision %d, current revision is %d", request.ExpectedRevision, state.RegistryRevision)
		}
		node := state.Nodes[nodeID]
		if node == nil {
			conflictCode = "device_not_found"
			return fmt.Errorf("device not found")
		}
		index := -1
		for i := range node.Descriptions {
			if node.Descriptions[i].Source == description.Source && node.Descriptions[i].Author == description.Author {
				index = i
				break
			}
		}
		if index < 0 {
			node.Descriptions = append(node.Descriptions, description)
			created = true
		} else {
			node.Descriptions[index] = description
		}
		node.Descriptions = sortedDeviceDescriptions(node.Descriptions)
		node.UpdatedAt = now
		revision = bumpRegistry(state, s.cfg.EventRetention, "device.description_updated", "device", nodeID, map[string]string{"source": description.Source, "author": description.Author})
		addEvent(state, s.cfg.EventRetention, "device.description_updated", actorFromRequest(r), "device", nodeID, "device description contribution updated", map[string]string{"source": description.Source, "author": description.Author})
		snapshot = deviceSnapshotLocked(state, node, now)
		return nil
	})
	if err != nil {
		switch conflictCode {
		case "stale_registry_revision":
			writeAPIError(w, http.StatusConflict, conflictCode, err.Error())
		case "device_not_found":
			writeAPIError(w, http.StatusNotFound, conflictCode, err.Error())
		default:
			writeAPIError(w, http.StatusInternalServerError, "device_description_failed", err.Error())
		}
		return
	}
	s.notifyRegistryChanged()
	writeJSON(w, http.StatusOK, map[string]any{"created": created, "revision": revision, "device": snapshot})
}

func (s *Server) handleDeviceDescriptionRemove(w http.ResponseWriter, r *http.Request) {
	if !s.requireAllocator(w, r) {
		return
	}
	var request deviceDescriptionRemoveRequest
	if !decodeJSON(w, r, s.cfg.MaxRequestBytes, &request) {
		return
	}
	source, author, err := validateDeviceDescriptionIdentity(request.Source, request.Author)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_device_description", err.Error())
		return
	}
	if source == core.DeviceDescriptionSourceHuman && !isAdminRequest(r) {
		writeAPIError(w, http.StatusForbidden, "admin_required", "only an admin may remove a human device description")
		return
	}
	nodeID := r.PathValue("nodeID")
	now := time.Now().UTC()
	var snapshot deviceSnapshot
	var revision int64
	conflictCode := ""
	err = s.store.Update(func(state *core.State) error {
		if request.ExpectedRevision != state.RegistryRevision {
			conflictCode = "stale_registry_revision"
			return fmt.Errorf("expected revision %d, current revision is %d", request.ExpectedRevision, state.RegistryRevision)
		}
		node := state.Nodes[nodeID]
		if node == nil {
			conflictCode = "device_not_found"
			return fmt.Errorf("device not found")
		}
		index := -1
		for i := range node.Descriptions {
			if node.Descriptions[i].Source == source && node.Descriptions[i].Author == author {
				index = i
				break
			}
		}
		if index < 0 {
			conflictCode = "device_description_not_found"
			return fmt.Errorf("device description not found")
		}
		node.Descriptions = append(node.Descriptions[:index], node.Descriptions[index+1:]...)
		node.UpdatedAt = now
		revision = bumpRegistry(state, s.cfg.EventRetention, "device.description_removed", "device", nodeID, map[string]string{"source": source, "author": author})
		addEvent(state, s.cfg.EventRetention, "device.description_removed", actorFromRequest(r), "device", nodeID, "device description contribution removed", map[string]string{"source": source, "author": author})
		snapshot = deviceSnapshotLocked(state, node, now)
		return nil
	})
	if err != nil {
		switch conflictCode {
		case "stale_registry_revision":
			writeAPIError(w, http.StatusConflict, conflictCode, err.Error())
		case "device_not_found", "device_description_not_found":
			writeAPIError(w, http.StatusNotFound, conflictCode, err.Error())
		default:
			writeAPIError(w, http.StatusInternalServerError, "device_description_failed", err.Error())
		}
		return
	}
	s.notifyRegistryChanged()
	writeJSON(w, http.StatusOK, map[string]any{"removed": true, "revision": revision, "device": snapshot})
}
