package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/qiyuehuajin/agentspool/internal/client"
	"github.com/qiyuehuajin/agentspool/internal/core"
)

type toolCallOptions struct {
	server                 string
	adminToken             string
	adminTokenFile         string
	allocatorToken         string
	allocatorTokenFile     string
	resourceLeaseToken     string
	resourceLeaseTokenFile string
	sandboxToken           string
	sandboxTokenFile       string
	timeout                time.Duration
	insecure               bool
	input                  string
}

func runTool(args []string) error {
	if len(args) == 0 {
		return errors.New("tool requires list, describe, or a tool name")
	}
	name := args[0]
	if name == "list" {
		return runToolList(args[1:])
	}
	if name == "describe" {
		return runToolDescribe(args[1:])
	}
	definition, ok := registeredTools[name]
	if !ok {
		return fmt.Errorf("unknown Agent tool %q; use 'agentspool tool list --json' to discover tools", name)
	}
	profile, _, err := loadClientProfile()
	if err != nil {
		return err
	}
	serverDefault := profile.Server
	if serverDefault == "" {
		serverDefault = "http://127.0.0.1:8090"
	}
	if value := strings.TrimSpace(os.Getenv("AGENTSPOOL_SERVER")); value != "" {
		serverDefault = value
	}
	adminTokenFileDefault := profile.AdminTokenFile
	if value := strings.TrimSpace(os.Getenv("AGENTSPOOL_ADMIN_TOKEN_FILE")); value != "" {
		adminTokenFileDefault = value
	}
	allocatorTokenFileDefault := profile.AllocatorTokenFile
	if value := strings.TrimSpace(os.Getenv("AGENTSPOOL_ALLOCATOR_TOKEN_FILE")); value != "" {
		allocatorTokenFileDefault = value
	}
	fs := flag.NewFlagSet("tool "+name, flag.ContinueOnError)
	options := toolCallOptions{}
	fs.StringVar(&options.server, "server", serverDefault, "control-plane URL")
	fs.StringVar(&options.adminToken, "admin-token", os.Getenv("AGENTSPOOL_ADMIN_TOKEN"), "admin bearer token")
	fs.StringVar(&options.adminTokenFile, "admin-token-file", adminTokenFileDefault, "file containing the admin bearer token")
	fs.StringVar(&options.allocatorToken, "allocator-token", os.Getenv("AGENTSPOOL_ALLOCATOR_TOKEN"), "allocator bearer token")
	fs.StringVar(&options.allocatorTokenFile, "allocator-token-file", allocatorTokenFileDefault, "file containing the allocator bearer token")
	fs.StringVar(&options.resourceLeaseToken, "resource-lease-token", os.Getenv("AGENTSPOOL_RESOURCE_LEASE_TOKEN"), "scoped resource lease token")
	fs.StringVar(&options.resourceLeaseTokenFile, "resource-lease-token-file", os.Getenv("AGENTSPOOL_RESOURCE_LEASE_TOKEN_FILE"), "file containing the scoped resource lease token")
	fs.StringVar(&options.sandboxToken, "sandbox-token", os.Getenv("AGENTSPOOL_SANDBOX_TOKEN"), "scoped sandbox token")
	fs.StringVar(&options.sandboxTokenFile, "sandbox-token-file", os.Getenv("AGENTSPOOL_SANDBOX_TOKEN_FILE"), "file containing the scoped sandbox token")
	fs.DurationVar(&options.timeout, "timeout", 0, "optional request and operation wait timeout; 0 disables the deadline")
	fs.BoolVar(&options.insecure, "insecure-skip-tls-verify", false, "disable TLS certificate verification")
	fs.StringVar(&options.input, "input", "{}", "JSON object, @FILE, or - for stdin")
	if err := parseInterspersed(fs, args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %s", strings.Join(fs.Args(), " "))
	}
	if options.timeout < 0 {
		return errors.New("--timeout must be zero or greater")
	}
	if options.resourceLeaseToken, err = resolveSecret(options.resourceLeaseToken, options.resourceLeaseTokenFile, "resource lease"); err != nil {
		return err
	}
	if options.sandboxToken, err = resolveSecret(options.sandboxToken, options.sandboxTokenFile, "sandbox"); err != nil {
		return err
	}
	// Once a scoped credential is present, do not even read the admin token
	// file. This is a real privilege downgrade, not merely header filtering.
	if options.resourceLeaseToken != "" || options.sandboxToken != "" {
		options.adminToken = ""
		options.allocatorToken = ""
	} else {
		allowsAllocator := false
		for _, alternative := range definition.Descriptor.Auth.Alternatives {
			if len(alternative) == 1 && alternative[0] == "allocator" {
				allowsAllocator = true
				break
			}
		}
		if allowsAllocator && (options.allocatorToken != "" || options.allocatorTokenFile != "") {
			if options.allocatorToken, err = resolveSecret(options.allocatorToken, options.allocatorTokenFile, "allocator"); err != nil {
				return err
			}
			options.adminToken = ""
		} else if options.adminToken, err = resolveSecret(options.adminToken, options.adminTokenFile, "admin"); err != nil {
			return err
		}
	}
	input, fields, err := readToolInput(options.input)
	if err != nil {
		return err
	}
	bearer := options.adminToken
	if options.allocatorToken != "" {
		bearer = options.allocatorToken
	}
	api, err := client.New(options.server, bearer, options.timeout, options.insecure)
	if err != nil {
		return err
	}
	headers := map[string]string{}
	if options.resourceLeaseToken != "" {
		headers["X-AgentsPool-Resource-Lease-Token"] = options.resourceLeaseToken
	}
	if options.sandboxToken != "" {
		headers["X-AgentsPool-Sandbox-Token"] = options.sandboxToken
	}
	ctx := context.Background()
	cancel := func() {}
	if options.timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, options.timeout)
	}
	defer cancel()
	switch definition.Execution {
	case toolExecutionPush:
		return runToolFilePush(ctx, api, fields, headers)
	case toolExecutionPull:
		return runToolFilePull(ctx, api, fields, headers)
	}
	method, endpoint, query, body, err := resolveToolCall(name, input, fields)
	if err != nil {
		return err
	}
	if definition.Execution == toolExecutionWait {
		return waitForSandboxOperation(ctx, api, endpoint, headers)
	}
	var response any
	if err := api.DoJSON(ctx, method, endpoint, query, body, &response, headers); err != nil {
		return err
	}
	return printJSON(response)
}

func readToolInput(value string) (json.RawMessage, map[string]json.RawMessage, error) {
	var data []byte
	var err error
	switch {
	case value == "-":
		data, err = readInput("-")
	case strings.HasPrefix(value, "@"):
		if len(value) == 1 {
			return nil, nil, errors.New("--input @FILE requires a file path")
		}
		data, err = os.ReadFile(value[1:])
	default:
		data = []byte(value)
	}
	if err != nil {
		return nil, nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, nil, fmt.Errorf("decode tool input: %w", err)
	}
	if fields == nil {
		return nil, nil, errors.New("tool input must be a JSON object")
	}
	return json.RawMessage(data), fields, nil
}

func toolString(fields map[string]json.RawMessage, name string) (string, error) {
	raw, ok := fields[name]
	if !ok {
		return "", fmt.Errorf("tool input requires %q", name)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("tool input %q must be a non-empty string", name)
	}
	return strings.TrimSpace(value), nil
}

func toolInt(fields map[string]json.RawMessage, name string, fallback int64) (int64, error) {
	raw, ok := fields[name]
	if !ok {
		return fallback, nil
	}
	var value int64
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, fmt.Errorf("tool input %q must be an integer", name)
	}
	return value, nil
}

func resolveToolCall(name string, input json.RawMessage, fields map[string]json.RawMessage) (string, string, url.Values, any, error) {
	definition, ok := registeredTools[name]
	if !ok || definition.Resolve == nil {
		return "", "", nil, nil, fmt.Errorf("unknown Agent tool %q", name)
	}
	return definition.Resolve(input, fields)
}

func toolBody(fields map[string]json.RawMessage, omitted ...string) map[string]json.RawMessage {
	skip := make(map[string]bool, len(omitted))
	for _, key := range omitted {
		skip[key] = true
	}
	body := make(map[string]json.RawMessage, len(fields))
	for key, value := range fields {
		if !skip[key] {
			body[key] = value
		}
	}
	return body
}

func waitForSandboxOperation(ctx context.Context, api *client.Client, endpoint string, headers map[string]string) error {
	response, err := waitSandboxOperation(ctx, api, endpoint, headers)
	if err != nil {
		if response.Operation.ID != "" {
			_ = printJSON(response)
		}
		return err
	}
	return printJSON(response)
}

type sandboxOperationResponse struct {
	Operation core.SandboxOperation `json:"operation"`
}

func waitSandboxOperation(ctx context.Context, api *client.Client, endpoint string, headers map[string]string) (sandboxOperationResponse, error) {
	for {
		var response sandboxOperationResponse
		if err := api.DoJSON(ctx, http.MethodGet, endpoint, nil, nil, &response, headers); err != nil {
			return response, err
		}
		switch response.Operation.Status {
		case core.SandboxOperationSucceeded:
			return response, nil
		case core.SandboxOperationFailed:
			return response, fmt.Errorf("sandbox operation failed: %s", response.Operation.Error)
		case core.SandboxOperationCancelled:
			return response, errors.New("sandbox operation was cancelled")
		}
		select {
		case <-ctx.Done():
			return response, ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
}

func runToolFilePush(ctx context.Context, api *client.Client, fields map[string]json.RawMessage, headers map[string]string) error {
	sandboxID, err := toolString(fields, "sandbox_id")
	if err != nil {
		return err
	}
	localFile, err := toolString(fields, "local_file")
	if err != nil {
		return err
	}
	targetPath, err := toolString(fields, "path")
	if err != nil {
		return err
	}
	query := url.Values{"path": {targetPath}}
	if raw, ok := fields["executable"]; ok {
		var executable bool
		if err := json.Unmarshal(raw, &executable); err != nil {
			return errors.New("tool input executable must be boolean")
		}
		query.Set("executable", strconv.FormatBool(executable))
	}
	if raw, ok := fields["media_type"]; ok {
		var mediaType string
		if err := json.Unmarshal(raw, &mediaType); err != nil {
			return errors.New("tool input media_type must be a string")
		}
		query.Set("media_type", mediaType)
	}
	var response struct {
		Artifact  core.Artifact         `json:"artifact"`
		Operation core.SandboxOperation `json:"operation"`
	}
	if err := api.UploadFile(ctx, "/api/v1/sandboxes/"+url.PathEscape(sandboxID)+"/files", query, localFile, headers, &response); err != nil {
		return err
	}
	wait, err := waitSandboxOperation(ctx, api, "/api/v1/sandbox-operations/"+url.PathEscape(response.Operation.ID), headers)
	if err != nil {
		return err
	}
	return printJSON(map[string]any{"artifact": response.Artifact, "operation": wait.Operation})
}

func runToolFilePull(ctx context.Context, api *client.Client, fields map[string]json.RawMessage, headers map[string]string) error {
	sandboxID, err := toolString(fields, "sandbox_id")
	if err != nil {
		return err
	}
	filePath, err := toolString(fields, "path")
	if err != nil {
		return err
	}
	output, err := toolString(fields, "output")
	if err != nil {
		return err
	}
	var queued sandboxOperationResponse
	if err := api.DoJSON(ctx, http.MethodPost, "/api/v1/sandboxes/"+url.PathEscape(sandboxID)+"/files/export", nil, map[string]string{"path": filePath}, &queued, headers); err != nil {
		return err
	}
	finished, err := waitSandboxOperation(ctx, api, "/api/v1/sandbox-operations/"+url.PathEscape(queued.Operation.ID), headers)
	if err != nil {
		return err
	}
	if finished.Operation.ResultArtifactID == "" {
		return errors.New("sandbox export completed without an artifact")
	}
	if err := api.Download(ctx, "/api/v1/sandboxes/"+url.PathEscape(sandboxID)+"/artifacts/"+url.PathEscape(finished.Operation.ResultArtifactID)+"/content", nil, output, headers); err != nil {
		return err
	}
	return printJSON(map[string]any{"output": output, "artifact_id": finished.Operation.ResultArtifactID, "operation": finished.Operation})
}
