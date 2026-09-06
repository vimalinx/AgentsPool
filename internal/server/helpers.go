package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/vimalinx/AgentsPool/internal/core"
)

type contextKey string

const actorContextKey contextKey = "actor"

type APIError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeAPIError(w http.ResponseWriter, status int, code, message string) {
	var response APIError
	response.Error.Code = code
	response.Error.Message = message
	writeJSON(w, status, response)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, maxBytes int64, target any) bool {
	if maxBytes > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		var syntaxError *json.SyntaxError
		var typeError *json.UnmarshalTypeError
		switch {
		case errors.As(err, &syntaxError):
			writeAPIError(w, http.StatusBadRequest, "invalid_json", fmt.Sprintf("invalid JSON at byte %d", syntaxError.Offset))
		case errors.As(err, &typeError):
			writeAPIError(w, http.StatusBadRequest, "invalid_json_type", fmt.Sprintf("invalid value for field %s", typeError.Field))
		case errors.Is(err, io.EOF):
			writeAPIError(w, http.StatusBadRequest, "empty_body", "request body is required")
		default:
			writeAPIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		}
		return false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeAPIError(w, http.StatusBadRequest, "multiple_json_values", "request body must contain exactly one JSON value")
		return false
	}
	return true
}

func parseBearer(r *http.Request) string {
	header := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(header) < 8 || !strings.EqualFold(header[:7], "Bearer ") {
		return ""
	}
	return strings.TrimSpace(header[7:])
}

func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	token := parseBearer(r)
	if token == "" || !core.SecureHashEqual(core.HashSecret(s.cfg.AdminToken), token) {
		s.metrics.AuthenticationFails.Add(1)
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeAPIError(w, http.StatusUnauthorized, "unauthorized", "a valid admin bearer token is required")
		return false
	}
	*r = *r.WithContext(context.WithValue(r.Context(), actorContextKey, "admin"))
	return true
}

func (s *Server) requireAllocator(w http.ResponseWriter, r *http.Request) bool {
	token := parseBearer(r)
	actor := ""
	switch {
	case token != "" && core.SecureHashEqual(core.HashSecret(s.cfg.AdminToken), token):
		actor = "admin"
	case s.cfg.AllocatorToken != "" && token != "" && core.SecureHashEqual(core.HashSecret(s.cfg.AllocatorToken), token):
		actor = "allocator"
	}
	if actor == "" {
		s.metrics.AuthenticationFails.Add(1)
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeAPIError(w, http.StatusUnauthorized, "unauthorized", "a valid allocator or admin bearer token is required")
		return false
	}
	*r = *r.WithContext(context.WithValue(r.Context(), actorContextKey, actor))
	return true
}

func isAdminRequest(r *http.Request) bool {
	actor, _ := r.Context().Value(actorContextKey).(string)
	return actor == "admin"
}

func (s *Server) authenticateNode(w http.ResponseWriter, r *http.Request, expectedNodeID string) (*core.Node, bool) {
	token := parseBearer(r)
	if token == "" {
		s.metrics.AuthenticationFails.Add(1)
		writeAPIError(w, http.StatusUnauthorized, "unauthorized", "a node bearer token is required")
		return nil, false
	}
	var nodeCopy *core.Node
	_ = s.store.View(func(state *core.State) error {
		node := state.Nodes[expectedNodeID]
		if node != nil && core.SecureHashEqual(node.TokenHash, token) {
			copy := *node
			nodeCopy = &copy
		}
		return nil
	})
	if nodeCopy == nil {
		s.metrics.AuthenticationFails.Add(1)
		writeAPIError(w, http.StatusUnauthorized, "unauthorized", "invalid node credentials")
		return nil, false
	}
	if nodeCopy.Status == core.NodeDisabled {
		writeAPIError(w, http.StatusForbidden, "node_disabled", "node is disabled")
		return nil, false
	}
	*r = *r.WithContext(context.WithValue(r.Context(), actorContextKey, "node:"+expectedNodeID))
	return nodeCopy, true
}

func (s *Server) authenticateAdminOrNode(w http.ResponseWriter, r *http.Request) bool {
	token := parseBearer(r)
	if token == "" {
		writeAPIError(w, http.StatusUnauthorized, "unauthorized", "bearer token is required")
		return false
	}
	if core.SecureHashEqual(core.HashSecret(s.cfg.AdminToken), token) {
		*r = *r.WithContext(context.WithValue(r.Context(), actorContextKey, "admin"))
		return true
	}
	valid := false
	_ = s.store.View(func(state *core.State) error {
		for _, node := range state.Nodes {
			if node.Status != core.NodeDisabled && core.SecureHashEqual(node.TokenHash, token) {
				valid = true
				break
			}
		}
		return nil
	})
	if !valid {
		s.metrics.AuthenticationFails.Add(1)
		writeAPIError(w, http.StatusUnauthorized, "unauthorized", "invalid credentials")
		return false
	}
	return true
}

func (s *Server) authenticateLease(w http.ResponseWriter, r *http.Request, leaseID string) (*core.Lease, bool) {
	leaseToken := strings.TrimSpace(r.Header.Get("X-AgentsPool-Lease-Token"))
	if leaseToken == "" {
		writeAPIError(w, http.StatusUnauthorized, "lease_token_required", "X-AgentsPool-Lease-Token is required")
		return nil, false
	}
	var leaseCopy *core.Lease
	_ = s.store.View(func(state *core.State) error {
		lease := state.Leases[leaseID]
		if lease != nil && core.SecureHashEqual(lease.TokenHash, leaseToken) {
			copy := *lease
			leaseCopy = &copy
		}
		return nil
	})
	if leaseCopy == nil {
		writeAPIError(w, http.StatusUnauthorized, "invalid_lease_token", "invalid lease credentials")
		return nil, false
	}
	if _, ok := s.authenticateNode(w, r, leaseCopy.NodeID); !ok {
		return nil, false
	}
	return leaseCopy, true
}

func actorFromRequest(r *http.Request) string {
	if actor, ok := r.Context().Value(actorContextKey).(string); ok && actor != "" {
		return actor
	}
	return "unknown"
}

func newIDOrFallback(prefix string) string {
	id, err := core.NewID(prefix)
	if err != nil {
		return prefix + "_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return id
}

func addEvent(state *core.State, retention int, eventType, actor, resourceType, resourceID, message string, data map[string]string) {
	event := core.Event{
		ID:           newIDOrFallback("evt"),
		Timestamp:    time.Now().UTC(),
		Type:         eventType,
		Actor:        actor,
		ResourceType: resourceType,
		ResourceID:   resourceID,
		Message:      message,
		Data:         data,
	}
	state.Events = append(state.Events, event)
	if retention > 0 && len(state.Events) > retention {
		state.Events = append([]core.Event(nil), state.Events[len(state.Events)-retention:]...)
	}
}

func bumpRegistry(state *core.State, retention int, eventType, resourceType, resourceID string, data map[string]string) int64 {
	state.RegistryRevision++
	event := core.RegistryEvent{
		Revision: state.RegistryRevision, Timestamp: time.Now().UTC(), Type: eventType,
		ResourceType: resourceType, ResourceID: resourceID, Data: data,
	}
	state.RegistryEvents = append(state.RegistryEvents, event)
	if retention > 0 && len(state.RegistryEvents) > retention {
		state.RegistryEvents = append([]core.RegistryEvent(nil), state.RegistryEvents[len(state.RegistryEvents)-retention:]...)
	}
	return state.RegistryRevision
}

func (s *Server) authenticateResourceLease(w http.ResponseWriter, r *http.Request, leaseID string) (*core.ResourceLease, bool) {
	bearer := parseBearer(r)
	leaseToken := strings.TrimSpace(r.Header.Get("X-AgentsPool-Resource-Lease-Token"))
	var leaseCopy *core.ResourceLease
	_ = s.store.View(func(state *core.State) error {
		lease := state.ResourceLeases[leaseID]
		if lease == nil {
			return nil
		}
		if core.SecureHashEqual(core.HashSecret(s.cfg.AdminToken), bearer) || (leaseToken != "" && core.SecureHashEqual(lease.TokenHash, leaseToken)) {
			copy := cloneViaJSON(*lease)
			leaseCopy = &copy
		}
		return nil
	})
	if leaseCopy == nil {
		s.metrics.AuthenticationFails.Add(1)
		writeAPIError(w, http.StatusUnauthorized, "invalid_resource_lease_token", "admin bearer token or matching resource lease token is required")
		return nil, false
	}
	return leaseCopy, true
}

func (s *Server) authenticateSandbox(w http.ResponseWriter, r *http.Request, sandboxID string) (*core.Sandbox, bool) {
	bearer := parseBearer(r)
	sandboxToken := strings.TrimSpace(r.Header.Get("X-AgentsPool-Sandbox-Token"))
	var sandboxCopy *core.Sandbox
	_ = s.store.View(func(state *core.State) error {
		sandbox := state.Sandboxes[sandboxID]
		if sandbox == nil {
			return nil
		}
		if core.SecureHashEqual(core.HashSecret(s.cfg.AdminToken), bearer) || (sandboxToken != "" && core.SecureHashEqual(sandbox.TokenHash, sandboxToken)) {
			copy := cloneViaJSON(*sandbox)
			sandboxCopy = &copy
		}
		return nil
	})
	if sandboxCopy == nil {
		s.metrics.AuthenticationFails.Add(1)
		writeAPIError(w, http.StatusUnauthorized, "invalid_sandbox_token", "admin bearer token or matching sandbox token is required")
		return nil, false
	}
	return sandboxCopy, true
}

func cloneViaJSON[T any](value T) T {
	data, _ := json.Marshal(value)
	var out T
	_ = json.Unmarshal(data, &out)
	return out
}

func publicNode(node *core.Node) map[string]any {
	return map[string]any{
		"id":            node.ID,
		"machine_id":    node.MachineID,
		"name":          node.Name,
		"status":        node.Status,
		"capabilities":  node.Capabilities,
		"resources":     node.Resources,
		"descriptions":  sortedDeviceDescriptions(node.Descriptions),
		"registered_at": node.RegisteredAt,
		"last_seen_at":  node.LastSeenAt,
		"updated_at":    node.UpdatedAt,
		"disabled_at":   node.DisabledAt,
	}
}

func sanitizeFilename(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	name = strings.ReplaceAll(name, "\x00", "")
	if name == "." || name == "" || name == string(filepath.Separator) {
		return "artifact.bin"
	}
	if len(name) > 240 {
		name = name[:240]
	}
	return name
}

func contentDisposition(name string) string {
	return mime.FormatMediaType("attachment", map[string]string{"filename": sanitizeFilename(name)})
}

func parseLimit(r *http.Request, defaultValue, maxValue int) int {
	value, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || value <= 0 {
		return defaultValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
}
