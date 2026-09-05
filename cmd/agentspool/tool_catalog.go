package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

const (
	toolCatalogVersion = "4"
	toolSchemaDialect  = "https://json-schema.org/draft/2020-12/schema"
	toolAuthSemantics  = "each inner list is an all-of credential set; any alternative may authorize the call"
)

type toolJSONSchema struct {
	Type                 string                    `json:"type"`
	Description          string                    `json:"description,omitempty"`
	Enum                 []string                  `json:"enum,omitempty"`
	Required             []string                  `json:"required,omitempty"`
	Properties           map[string]toolJSONSchema `json:"properties,omitempty"`
	Items                *toolJSONSchema           `json:"items,omitempty"`
	Minimum              *int64                    `json:"minimum,omitempty"`
	Maximum              *int64                    `json:"maximum,omitempty"`
	MaxLength            *int64                    `json:"maxLength,omitempty"`
	MaxItems             *int64                    `json:"maxItems,omitempty"`
	AdditionalProperties any                       `json:"additionalProperties,omitempty"`
}

type toolAuth struct {
	// Each inner list is an AND set. An invocation may use any one alternative.
	Alternatives [][]string `json:"alternatives"`
}

type toolDescriptor struct {
	Name              string         `json:"name"`
	Category          string         `json:"category"`
	Description       string         `json:"description"`
	Auth              toolAuth       `json:"auth"`
	Mutating          bool           `json:"mutating"`
	WaitsForOperation bool           `json:"waits_for_operation,omitempty"`
	InputSchema       toolJSONSchema `json:"input_schema"`
	ExampleInput      map[string]any `json:"example_input"`
}

type toolSummary struct {
	Name        string   `json:"name"`
	Category    string   `json:"category"`
	Description string   `json:"description"`
	Auth        toolAuth `json:"auth"`
	Mutating    bool     `json:"mutating"`
}

type toolResolver func(json.RawMessage, map[string]json.RawMessage) (string, string, url.Values, any, error)

type toolDefinition struct {
	Descriptor toolDescriptor
	Execution  string
	Resolve    toolResolver
}

const (
	toolExecutionHTTP = "http"
	toolExecutionWait = "operation_wait"
	toolExecutionPush = "file_push"
	toolExecutionPull = "file_pull"
)

var registeredTools = buildToolCatalog()

func int64Pointer(value int64) *int64 { return &value }

func objectSchema(properties map[string]toolJSONSchema, required ...string) toolJSONSchema {
	return toolJSONSchema{Type: "object", Properties: properties, Required: required, AdditionalProperties: false}
}

func stringSchema(description string) toolJSONSchema {
	return toolJSONSchema{Type: "string", Description: description}
}

func enumStringSchema(description string, values ...string) toolJSONSchema {
	return toolJSONSchema{Type: "string", Description: description, Enum: values}
}

func stringArraySchema(description string, maxItems, maxLength int64) toolJSONSchema {
	return toolJSONSchema{
		Type: "array", Description: description, MaxItems: int64Pointer(maxItems),
		Items: &toolJSONSchema{Type: "string", MaxLength: int64Pointer(maxLength)},
	}
}

func integerSchema(description string, minimum, maximum *int64) toolJSONSchema {
	return toolJSONSchema{Type: "integer", Description: description, Minimum: minimum, Maximum: maximum}
}

func boolSchema(description string) toolJSONSchema {
	return toolJSONSchema{Type: "boolean", Description: description}
}

func resourcesSchema() toolJSONSchema {
	return objectSchema(map[string]toolJSONSchema{
		"cpu_millis": integerSchema("CPU reservation in millicores.", int64Pointer(10), nil),
		"memory_mb":  integerSchema("Memory reservation in MiB.", int64Pointer(16), nil),
		"disk_mb":    integerSchema("Persistent disk reservation in MiB.", int64Pointer(32), nil),
	}, "cpu_millis", "memory_mb", "disk_mb")
}

func permissionsSchema() toolJSONSchema {
	return objectSchema(map[string]toolJSONSchema{
		"exec":       boolSchema("Allow command execution."),
		"data_read":  boolSchema("Allow reading persistent sandbox files."),
		"data_write": boolSchema("Allow writing persistent sandbox files; requires data_read."),
		"network":    boolSchema("Allow the sandbox to use the node network; false isolates networking."),
	}, "exec", "data_read", "data_write", "network")
}

func endpointsSchema() toolJSONSchema {
	item := objectSchema(map[string]toolJSONSchema{
		"name":       stringSchema("Stable endpoint name."),
		"protocol":   enumStringSchema("Published protocol.", "http", "https", "tcp"),
		"port":       integerSchema("Reserved node port.", int64Pointer(1), int64Pointer(65535)),
		"url":        stringSchema("Absolute LAN URL visible to other Agents."),
		"health_url": stringSchema("Optional absolute HTTP(S) health URL."),
	}, "name", "protocol", "port", "url")
	return toolJSONSchema{Type: "array", Description: "Endpoints published while this exec is running.", Items: &item}
}

func auth(alternatives ...[]string) toolAuth { return toolAuth{Alternatives: alternatives} }

func directCall(method, endpoint string, sendInput bool) toolResolver {
	return func(input json.RawMessage, _ map[string]json.RawMessage) (string, string, url.Values, any, error) {
		if sendInput {
			return method, endpoint, url.Values{}, input, nil
		}
		return method, endpoint, url.Values{}, nil, nil
	}
}

type pathBodyMode int

const (
	pathNoBody pathBodyMode = iota
	pathInputWithoutID
	pathEmptyObject
)

func pathCall(field, method, prefix, suffix string, mode pathBodyMode) toolResolver {
	return func(_ json.RawMessage, fields map[string]json.RawMessage) (string, string, url.Values, any, error) {
		id, err := toolString(fields, field)
		if err != nil {
			return "", "", nil, nil, err
		}
		var body any
		switch mode {
		case pathInputWithoutID:
			body = toolBody(fields, field)
		case pathEmptyObject:
			body = struct{}{}
		}
		return method, prefix + url.PathEscape(id) + suffix, url.Values{}, body, nil
	}
}

func descriptor(name, category, description string, authScopes toolAuth, mutating bool, schema toolJSONSchema, example map[string]any) toolDescriptor {
	return toolDescriptor{Name: name, Category: category, Description: description, Auth: authScopes, Mutating: mutating, InputSchema: schema, ExampleInput: example}
}

func buildToolCatalog() map[string]toolDefinition {
	admin := auth([]string{"admin"})
	allocator := auth([]string{"allocator"}, []string{"admin"})
	lease := auth([]string{"admin"}, []string{"resource_lease"})
	sandbox := auth([]string{"admin"}, []string{"sandbox"})
	rebind := auth([]string{"admin"}, []string{"resource_lease", "sandbox"})
	empty := objectSchema(map[string]toolJSONSchema{})
	nodeID := objectSchema(map[string]toolJSONSchema{"node_id": stringSchema("Device node ID.")}, "node_id")
	leaseID := objectSchema(map[string]toolJSONSchema{"lease_id": stringSchema("Resource lease ID.")}, "lease_id")
	sandboxID := objectSchema(map[string]toolJSONSchema{"sandbox_id": stringSchema("Sandbox ID.")}, "sandbox_id")
	operationID := objectSchema(map[string]toolJSONSchema{"operation_id": stringSchema("Sandbox operation ID.")}, "operation_id")
	artifactID := objectSchema(map[string]toolJSONSchema{"sandbox_id": stringSchema("Sandbox ID."), "artifact_id": stringSchema("Artifact ID owned by the sandbox.")}, "sandbox_id", "artifact_id")

	definitions := []toolDefinition{
		{descriptor("devices.list", "devices", "List current compute devices and the registry revision.", allocator, false, empty, map[string]any{}), toolExecutionHTTP, directCall(http.MethodGet, "/api/v1/devices", false)},
		{descriptor("devices.get", "devices", "Get one compute device and the registry revision.", allocator, false, nodeID, map[string]any{"node_id": "NODE_ID"}), toolExecutionHTTP, pathCall("node_id", http.MethodGet, "/api/v1/devices/", "", pathNoBody)},
		{descriptor("devices.watch", "devices", "Long-poll device changes after a registry revision.", allocator, false, objectSchema(map[string]toolJSONSchema{
			"since_revision": integerSchema("Last observed registry revision.", int64Pointer(0), nil),
			"wait_seconds":   integerSchema("Maximum long-poll duration.", int64Pointer(0), int64Pointer(30)),
		}), map[string]any{"since_revision": 0, "wait_seconds": 25}), toolExecutionHTTP, func(_ json.RawMessage, fields map[string]json.RawMessage) (string, string, url.Values, any, error) {
			revision, err := toolInt(fields, "since_revision", 0)
			if err != nil {
				return "", "", nil, nil, err
			}
			wait, err := toolInt(fields, "wait_seconds", 25)
			query := url.Values{"since_revision": {strconv.FormatInt(revision, 10)}, "wait_seconds": {strconv.FormatInt(wait, 10)}}
			return http.MethodGet, "/api/v1/devices-watch", query, nil, err
		}},
		{descriptor("devices.annotate", "devices", "Create or replace one human or Agent contribution to a device description; human contributions require admin while Agent contributions accept allocator.", allocator, true, objectSchema(map[string]toolJSONSchema{
			"node_id":             stringSchema("Device node ID."),
			"expected_revision":   integerSchema("Registry revision on which this description is based.", int64Pointer(0), nil),
			"source":              enumStringSchema("Contribution authority. Use human only after explicit human direction; Agent-authored recommendations use agent.", "human", "agent"),
			"author":              {Type: "string", Description: "Stable human or Agent contributor name.", MaxLength: int64Pointer(100)},
			"description":         {Type: "string", Description: "Plain-language purpose and operating context.", MaxLength: int64Pointer(2000)},
			"availability":        enumStringSchema("Expected availability, not live health.", "unknown", "always_on", "scheduled", "on_demand"),
			"availability_notes":  {Type: "string", Description: "Schedule, timezone, or wake-up details.", MaxLength: int64Pointer(1000)},
			"service_policy":      enumStringSchema("Whether long-running services belong on this device.", "unknown", "preferred", "allowed", "avoid", "forbidden"),
			"preferred_workloads": stringArraySchema("Workload kinds this contributor recommends.", 32, 100),
			"avoid_workloads":     stringArraySchema("Workload kinds this contributor recommends avoiding.", 32, 100),
			"constraints":         stringArraySchema("Human rules or Agent caveats relevant to placement.", 32, 500),
		}, "node_id", "expected_revision", "source", "author"), map[string]any{
			"node_id": "NODE_ID", "expected_revision": 1, "source": "human", "author": "owner",
			"description": "Always-on home server for durable services.", "availability": "always_on", "service_policy": "preferred",
			"preferred_workloads": []string{"long-running-service"}, "avoid_workloads": []string{"interactive-desktop"},
			"constraints": []string{"Do not interrupt existing services."},
		}), toolExecutionHTTP, pathCall("node_id", http.MethodPut, "/api/v1/devices/", "/descriptions", pathInputWithoutID)},
		{descriptor("devices.unannotate", "devices", "Remove one sourced contribution; human entries require admin while an Agent may remove its own allocator-authored entry.", allocator, true, objectSchema(map[string]toolJSONSchema{
			"node_id":           stringSchema("Device node ID."),
			"expected_revision": integerSchema("Current registry revision.", int64Pointer(0), nil),
			"source":            enumStringSchema("Contribution source to remove.", "human", "agent"),
			"author":            {Type: "string", Description: "Contributor name to remove.", MaxLength: int64Pointer(100)},
		}, "node_id", "expected_revision", "source", "author"), map[string]any{
			"node_id": "NODE_ID", "expected_revision": 2, "source": "agent", "author": "agent-name",
		}), toolExecutionHTTP, pathCall("node_id", http.MethodDelete, "/api/v1/devices/", "/descriptions", pathInputWithoutID)},
		{descriptor("services.list", "services", "List endpoints published by active sandbox exec Operations and probe their declared health URLs.", allocator, false, empty, map[string]any{}), toolExecutionHTTP, directCall(http.MethodGet, "/api/v1/services", false)},
		{descriptor("leases.acquire", "leases", "Reserve CPU, memory, and disk on an Agent-selected device.", allocator, true, objectSchema(map[string]toolJSONSchema{
			"node_id":           stringSchema("Selected device node ID."),
			"owner":             stringSchema("Opaque owner label; do not place secrets here."),
			"expected_revision": integerSchema("Registry revision used for placement.", int64Pointer(0), nil),
			"resources":         resourcesSchema(),
			"ttl_seconds":       integerSchema("Optional lease lifetime in seconds; omit or use 0 for no time deadline.", int64Pointer(0), nil),
		}, "node_id", "expected_revision", "resources"), map[string]any{"node_id": "NODE_ID", "owner": "agent-session", "expected_revision": 1, "resources": map[string]any{"cpu_millis": 500, "memory_mb": 512, "disk_mb": 256}}), toolExecutionHTTP, directCall(http.MethodPost, "/api/v1/resource-leases", true)},
		{descriptor("leases.list", "leases", "List all resource leases.", admin, false, empty, map[string]any{}), toolExecutionHTTP, directCall(http.MethodGet, "/api/v1/resource-leases", false)},
		{descriptor("leases.get", "leases", "Get one resource lease.", lease, false, leaseID, map[string]any{"lease_id": "LEASE_ID"}), toolExecutionHTTP, pathCall("lease_id", http.MethodGet, "/api/v1/resource-leases/", "", pathNoBody)},
		{descriptor("leases.renew", "leases", "Renew an active resource lease.", lease, true, objectSchema(map[string]toolJSONSchema{
			"lease_id":    stringSchema("Resource lease ID."),
			"ttl_seconds": integerSchema("New lease lifetime in seconds; omit to keep the current value or use 0 for no time deadline.", int64Pointer(0), nil),
		}, "lease_id"), map[string]any{"lease_id": "LEASE_ID", "ttl_seconds": 0}), toolExecutionHTTP, pathCall("lease_id", http.MethodPost, "/api/v1/resource-leases/", "/renew", pathInputWithoutID)},
		{descriptor("leases.resize", "leases", "Resize an active lease that is not attached to a sandbox.", lease, true, objectSchema(map[string]toolJSONSchema{
			"lease_id":          stringSchema("Resource lease ID."),
			"expected_revision": integerSchema("Current registry revision.", int64Pointer(0), nil),
			"resources":         resourcesSchema(),
		}, "lease_id", "expected_revision", "resources"), map[string]any{"lease_id": "LEASE_ID", "expected_revision": 2, "resources": map[string]any{"cpu_millis": 1000, "memory_mb": 1024, "disk_mb": 512}}), toolExecutionHTTP, pathCall("lease_id", http.MethodPost, "/api/v1/resource-leases/", "/resize", pathInputWithoutID)},
		{descriptor("leases.release", "leases", "Release a resource lease after its sandbox is destroyed or rebound.", lease, true, leaseID, map[string]any{"lease_id": "LEASE_ID"}), toolExecutionHTTP, pathCall("lease_id", http.MethodDelete, "/api/v1/resource-leases/", "", pathNoBody)},
		{descriptor("sandboxes.ensure", "sandboxes", "Create a scoped persistent sandbox on a resource lease.", lease, true, objectSchema(map[string]toolJSONSchema{
			"name":              stringSchema("Sandbox display name."),
			"resource_lease_id": stringSchema("Resource lease to attach."),
			"permissions":       permissionsSchema(),
		}, "name", "resource_lease_id", "permissions"), map[string]any{"name": "work-name", "resource_lease_id": "LEASE_ID", "permissions": map[string]any{"exec": true, "data_read": true, "data_write": true, "network": false}}), toolExecutionHTTP, directCall(http.MethodPost, "/api/v1/sandboxes", true)},
		{descriptor("sandboxes.list", "sandboxes", "List all persistent sandboxes.", admin, false, empty, map[string]any{}), toolExecutionHTTP, directCall(http.MethodGet, "/api/v1/sandboxes", false)},
		{descriptor("sandboxes.get", "sandboxes", "Get a sandbox and its persistent volumes.", sandbox, false, sandboxID, map[string]any{"sandbox_id": "SANDBOX_ID"}), toolExecutionHTTP, pathCall("sandbox_id", http.MethodGet, "/api/v1/sandboxes/", "", pathNoBody)},
		{descriptor("sandboxes.exec", "sandboxes", "Queue a command in a sandbox; inspect the returned operation for the result.", sandbox, true, objectSchema(map[string]toolJSONSchema{
			"sandbox_id":      stringSchema("Sandbox ID."),
			"command":         {Type: "array", Description: "Argument-vector command; no implicit shell parsing.", Items: &toolJSONSchema{Type: "string"}},
			"env":             {Type: "object", Description: "Non-secret environment values.", AdditionalProperties: toolJSONSchema{Type: "string"}},
			"working_dir":     stringSchema("Directory under /workspace or /home/agent."),
			"timeout_seconds": integerSchema("Optional execution timeout in seconds; omit or use 0 for no time deadline.", int64Pointer(0), nil),
			"endpoints":       endpointsSchema(),
		}, "sandbox_id", "command"), map[string]any{"sandbox_id": "SANDBOX_ID", "command": []string{"/bin/sh", "-c", "uname -a && pwd"}, "working_dir": "/workspace"}), toolExecutionHTTP, pathCall("sandbox_id", http.MethodPost, "/api/v1/sandboxes/", "/exec", pathInputWithoutID)},
		{descriptor("sandboxes.resize", "sandboxes", "Resize an attached sandbox lease and grow persistent disk when requested.", sandbox, true, objectSchema(map[string]toolJSONSchema{
			"sandbox_id":        stringSchema("Sandbox ID."),
			"expected_revision": integerSchema("Current registry revision.", int64Pointer(0), nil),
			"resources":         resourcesSchema(),
		}, "sandbox_id", "expected_revision", "resources"), map[string]any{"sandbox_id": "SANDBOX_ID", "expected_revision": 3, "resources": map[string]any{"cpu_millis": 1000, "memory_mb": 1024, "disk_mb": 512}}), toolExecutionHTTP, pathCall("sandbox_id", http.MethodPost, "/api/v1/sandboxes/", "/resize", pathInputWithoutID)},
		{descriptor("sandboxes.rebind", "sandboxes", "Attach a stopped persistent sandbox to a new lease on the same node.", rebind, true, objectSchema(map[string]toolJSONSchema{
			"sandbox_id":        stringSchema("Sandbox ID."),
			"resource_lease_id": stringSchema("Replacement resource lease ID."),
		}, "sandbox_id", "resource_lease_id"), map[string]any{"sandbox_id": "SANDBOX_ID", "resource_lease_id": "LEASE_ID"}), toolExecutionHTTP, pathCall("sandbox_id", http.MethodPost, "/api/v1/sandboxes/", "/rebind", pathInputWithoutID)},
		{descriptor("sandboxes.start", "sandboxes", "Start a stopped sandbox while its lease is active.", sandbox, true, sandboxID, map[string]any{"sandbox_id": "SANDBOX_ID"}), toolExecutionHTTP, pathCall("sandbox_id", http.MethodPost, "/api/v1/sandboxes/", "/start", pathEmptyObject)},
		{descriptor("sandboxes.stop", "sandboxes", "Stop a sandbox without deleting persistent data.", sandbox, true, sandboxID, map[string]any{"sandbox_id": "SANDBOX_ID"}), toolExecutionHTTP, pathCall("sandbox_id", http.MethodPost, "/api/v1/sandboxes/", "/stop", pathEmptyObject)},
		{descriptor("sandboxes.destroy", "sandboxes", "Destroy a sandbox and delete its persistent data.", sandbox, true, sandboxID, map[string]any{"sandbox_id": "SANDBOX_ID"}), toolExecutionHTTP, pathCall("sandbox_id", http.MethodDelete, "/api/v1/sandboxes/", "", pathNoBody)},
		{descriptor("operations.get", "operations", "Get the current state and result of a sandbox operation.", sandbox, false, operationID, map[string]any{"operation_id": "OPERATION_ID"}), toolExecutionHTTP, pathCall("operation_id", http.MethodGet, "/api/v1/sandbox-operations/", "", pathNoBody)},
		{descriptor("operations.logs", "operations", "Read live or completed sandbox output using a byte cursor; explicit rotation is reported as a gap.", sandbox, false, objectSchema(map[string]toolJSONSchema{
			"operation_id": stringSchema("Sandbox operation ID."),
			"stream":       enumStringSchema("Output stream; defaults to stdout.", "stdout", "stderr"),
			"after":        integerSchema("Byte offset from the preceding next_after; defaults to 0.", int64Pointer(0), nil),
			"limit":        integerSchema("Maximum bytes per page; defaults to 65536.", int64Pointer(1), int64Pointer(65536)),
		}, "operation_id"), map[string]any{"operation_id": "OPERATION_ID", "stream": "stdout", "after": 0}), toolExecutionHTTP, func(_ json.RawMessage, fields map[string]json.RawMessage) (string, string, url.Values, any, error) {
			id, err := toolString(fields, "operation_id")
			if err != nil {
				return "", "", nil, nil, err
			}
			query := url.Values{}
			for _, key := range []string{"after", "limit"} {
				if _, ok := fields[key]; !ok {
					continue
				}
				value, err := toolInt(fields, key, 0)
				if err != nil {
					return "", "", nil, nil, err
				}
				query.Set(key, strconv.FormatInt(value, 10))
			}
			if _, ok := fields["stream"]; ok {
				stream, err := toolString(fields, "stream")
				if err != nil {
					return "", "", nil, nil, err
				}
				query.Set("stream", stream)
			}
			return http.MethodGet, "/api/v1/sandbox-operations/" + url.PathEscape(id) + "/logs", query, nil, nil
		}},
		{descriptor("operations.cancel", "operations", "Request cancellation of a queued or running sandbox operation.", sandbox, true, operationID, map[string]any{"operation_id": "OPERATION_ID"}), toolExecutionHTTP, pathCall("operation_id", http.MethodPost, "/api/v1/sandbox-operations/", "/cancel", pathEmptyObject)},
		{func() toolDescriptor {
			value := descriptor("operations.wait", "operations", "Wait until a sandbox operation succeeds or fails; an optional CLI --timeout can bound the wait.", sandbox, false, operationID, map[string]any{"operation_id": "OPERATION_ID"})
			value.WaitsForOperation = true
			return value
		}(), toolExecutionWait, pathCall("operation_id", http.MethodGet, "/api/v1/sandbox-operations/", "", pathNoBody)},
		{func() toolDescriptor {
			value := descriptor("files.push", "files", "Upload a local file and place it inside a persistent sandbox volume.", sandbox, true, objectSchema(map[string]toolJSONSchema{
				"sandbox_id": stringSchema("Sandbox ID."), "local_file": stringSchema("Local source file path."), "path": stringSchema("Destination under /workspace or /home/agent."),
				"executable": boolSchema("Set the destination executable bit."), "media_type": stringSchema("Optional Artifact media type."),
			}, "sandbox_id", "local_file", "path"), map[string]any{"sandbox_id": "SANDBOX_ID", "local_file": "./input.dat", "path": "/workspace/input.dat"})
			value.WaitsForOperation = true
			return value
		}(), toolExecutionPush, nil},
		{func() toolDescriptor {
			value := descriptor("files.pull", "files", "Export a persistent sandbox file as an Artifact and download it locally.", sandbox, true, objectSchema(map[string]toolJSONSchema{
				"sandbox_id": stringSchema("Sandbox ID."), "path": stringSchema("Source under /workspace or /home/agent."), "output": stringSchema("Local destination file path."),
			}, "sandbox_id", "path", "output"), map[string]any{"sandbox_id": "SANDBOX_ID", "path": "/workspace/output.dat", "output": "./output.dat"})
			value.WaitsForOperation = true
			return value
		}(), toolExecutionPull, nil},
		{descriptor("artifacts.list", "artifacts", "List control-plane file bridge Artifacts owned by one sandbox.", sandbox, false, sandboxID, map[string]any{"sandbox_id": "SANDBOX_ID"}), toolExecutionHTTP, pathCall("sandbox_id", http.MethodGet, "/api/v1/sandboxes/", "/artifacts", pathNoBody)},
		{descriptor("artifacts.delete", "artifacts", "Delete one sandbox-owned Artifact after its operation is terminal; shared content remains while referenced.", sandbox, true, artifactID, map[string]any{"sandbox_id": "SANDBOX_ID", "artifact_id": "ARTIFACT_ID"}), toolExecutionHTTP, func(_ json.RawMessage, fields map[string]json.RawMessage) (string, string, url.Values, any, error) {
			sandboxID, err := toolString(fields, "sandbox_id")
			if err != nil {
				return "", "", nil, nil, err
			}
			artifactID, err := toolString(fields, "artifact_id")
			if err != nil {
				return "", "", nil, nil, err
			}
			return http.MethodDelete, "/api/v1/sandboxes/" + url.PathEscape(sandboxID) + "/artifacts/" + url.PathEscape(artifactID), url.Values{}, nil, nil
		}},
		{descriptor("maintenance.prune", "maintenance", "Preview or prune released leases, terminal sandbox Operations, stale sandbox Artifacts, and obsolete observation events without age or count limits.", admin, true, objectSchema(map[string]toolJSONSchema{
			"execute": boolSchema("Actually delete the reported terminal history; false is a dry run."),
		}), map[string]any{"execute": false}), toolExecutionHTTP, directCall(http.MethodPost, "/api/v1/maintenance/prune", true)},
	}

	registry := make(map[string]toolDefinition, len(definitions))
	for _, definition := range definitions {
		if definition.Descriptor.Name == "" {
			panic("AgentsPool tool has no name")
		}
		if _, exists := registry[definition.Descriptor.Name]; exists {
			panic("duplicate AgentsPool tool " + definition.Descriptor.Name)
		}
		registry[definition.Descriptor.Name] = definition
	}
	return registry
}

func sortedToolDefinitions(category string) []toolDefinition {
	definitions := make([]toolDefinition, 0, len(registeredTools))
	for _, definition := range registeredTools {
		if category == "" || definition.Descriptor.Category == category {
			definitions = append(definitions, definition)
		}
	}
	sort.Slice(definitions, func(i, j int) bool { return definitions[i].Descriptor.Name < definitions[j].Descriptor.Name })
	return definitions
}

func runToolList(args []string) error {
	fs := flag.NewFlagSet("tool list", flag.ContinueOnError)
	_ = fs.Bool("json", false, "emit machine-readable JSON (the default output format)")
	category := fs.String("category", "", "filter devices, services, leases, sandboxes, operations, files, artifacts, or maintenance")
	if err := parseInterspersed(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("tool list does not accept positional arguments")
	}
	if *category != "" {
		valid := map[string]bool{"devices": true, "services": true, "leases": true, "sandboxes": true, "operations": true, "files": true, "artifacts": true, "maintenance": true}
		if !valid[*category] {
			return fmt.Errorf("unknown tool category %q", *category)
		}
	}
	definitions := sortedToolDefinitions(*category)
	summaries := make([]toolSummary, 0, len(definitions))
	for _, definition := range definitions {
		descriptor := definition.Descriptor
		summaries = append(summaries, toolSummary{Name: descriptor.Name, Category: descriptor.Category, Description: descriptor.Description, Auth: descriptor.Auth, Mutating: descriptor.Mutating})
	}
	return printJSON(map[string]any{"catalog_version": toolCatalogVersion, "schema_dialect": toolSchemaDialect, "auth_semantics": toolAuthSemantics, "count": len(summaries), "tools": summaries})
}

func runToolDescribe(args []string) error {
	fs := flag.NewFlagSet("tool describe", flag.ContinueOnError)
	_ = fs.Bool("json", false, "emit machine-readable JSON (the default output format)")
	if err := parseInterspersed(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("tool describe requires exactly one TOOL_NAME")
	}
	name := strings.TrimSpace(fs.Arg(0))
	definition, ok := registeredTools[name]
	if !ok {
		return fmt.Errorf("unknown Agent tool %q", name)
	}
	return printJSON(map[string]any{"catalog_version": toolCatalogVersion, "schema_dialect": toolSchemaDialect, "auth_semantics": toolAuthSemantics, "tool": definition.Descriptor})
}
