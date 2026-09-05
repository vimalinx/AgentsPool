package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func disableClientProfile(t *testing.T) {
	t.Helper()
	t.Setenv("AGENTSPOOL_CONFIG", filepath.Join(t.TempDir(), "missing-client.json"))
}

func TestTaskSubmitRejectsRemovedGPUFieldsBeforeNetwork(t *testing.T) {
	taskFile := filepath.Join(t.TempDir(), "gpu-task.json")
	payload := `{"name":"legacy GPU task","requirements":{"gpu_count":1},"executor":{"kind":"shell","script":"true"}}`
	if err := os.WriteFile(taskFile, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	err := runTaskSubmit([]string{"--file", taskFile, "--server", "http://127.0.0.1:1", "--token", "admin-token-longer-than-24-characters"})
	if err == nil || !strings.Contains(err.Error(), "unknown field \"gpu_count\"") {
		t.Fatalf("expected strict GPU field rejection, got %v", err)
	}
}

func TestParseInterspersedAcceptsFlagsAfterPositionals(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	server := fs.String("server", "", "")
	follow := fs.Bool("follow", false, "")
	limit := fs.Int("limit", 0, "")

	args := []string{"task_123", "--follow", "--server", "http://router:8090", "--limit=25"}
	if err := parseInterspersed(fs, args); err != nil {
		t.Fatal(err)
	}
	if *server != "http://router:8090" || !*follow || *limit != 25 {
		t.Fatalf("flags not parsed: server=%q follow=%v limit=%d", *server, *follow, *limit)
	}
	if got, want := fs.Args(), []string{"task_123"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("positionals=%v, want %v", got, want)
	}
}

func TestParseInterspersedSupportsNegativeValuesAndDelimiter(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	priority := fs.Int("priority", 0, "")

	args := []string{"first", "--priority", "-5", "--", "--literal", "second"}
	if err := parseInterspersed(fs, args); err != nil {
		t.Fatal(err)
	}
	if *priority != -5 {
		t.Fatalf("priority=%d, want -5", *priority)
	}
	if got, want := fs.Args(), []string{"first", "--literal", "second"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("positionals=%v, want %v", got, want)
	}
}

func TestParseInterspersedRejectsUnknownFlag(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if err := parseInterspersed(fs, []string{"task_123", "--unknown"}); err == nil {
		t.Fatal("expected unknown flag error")
	}
}

func TestScopedToolOmitsAdminBearer(t *testing.T) {
	disableClientProfile(t)
	received := make(chan [2]string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- [2]string{r.Header.Get("Authorization"), r.Header.Get("X-AgentsPool-Resource-Lease-Token")}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"lease":{"id":"lease_test"}}`)
	}))
	defer server.Close()

	err := runTool([]string{
		"leases.get", "--server", server.URL,
		"--admin-token", "admin-token-must-not-be-sent",
		"--resource-lease-token", "scoped-lease-token",
		"--input", `{"lease_id":"lease_test"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	headers := <-received
	if headers[0] != "" || headers[1] != "scoped-lease-token" {
		t.Fatalf("unexpected scoped headers: authorization=%q resource_lease=%q", headers[0], headers[1])
	}
}

func TestOperationWaitHonorsOverallTimeout(t *testing.T) {
	disableClientProfile(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"operation":{"id":"op_test","status":"queued"}}`)
	}))
	defer server.Close()

	started := time.Now()
	err := runTool([]string{
		"operations.wait", "--server", server.URL,
		"--sandbox-token", "scoped-sandbox-token",
		"--timeout", "50ms",
		"--input", `{"operation_id":"op_test"}`,
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context deadline, got %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("operation wait ignored timeout: %s", elapsed)
	}
}

func TestToolCatalogExamplesResolveFromOneRegistry(t *testing.T) {
	if len(registeredTools) != 30 {
		t.Fatalf("tool catalog contains %d tools, want 30", len(registeredTools))
	}
	for name, definition := range registeredTools {
		t.Run(name, func(t *testing.T) {
			descriptor := definition.Descriptor
			if descriptor.Name != name || descriptor.Category == "" || descriptor.Description == "" {
				t.Fatalf("incomplete descriptor: %#v", descriptor)
			}
			if descriptor.InputSchema.Type != "object" || descriptor.InputSchema.AdditionalProperties != false {
				t.Fatalf("tool input schema is not a closed object: %#v", descriptor.InputSchema)
			}
			for _, required := range descriptor.InputSchema.Required {
				if _, ok := descriptor.InputSchema.Properties[required]; !ok {
					t.Fatalf("required input %q has no property schema", required)
				}
				if _, ok := descriptor.ExampleInput[required]; !ok {
					t.Fatalf("example input is missing required property %q", required)
				}
			}
			if definition.Execution == toolExecutionPush || definition.Execution == toolExecutionPull {
				if definition.Resolve != nil {
					t.Fatal("file transfer tool unexpectedly has an HTTP resolver")
				}
				return
			}
			encoded, err := json.Marshal(descriptor.ExampleInput)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(encoded, &fields); err != nil {
				t.Fatal(err)
			}
			method, endpoint, _, _, err := definition.Resolve(encoded, fields)
			if err != nil || method == "" || endpoint == "" {
				t.Fatalf("example does not resolve: method=%q endpoint=%q err=%v", method, endpoint, err)
			}
		})
	}
}

func TestToolCatalogHasNoLeaseOrExecTimeCeiling(t *testing.T) {
	cases := []struct {
		tool     string
		property string
	}{
		{tool: "leases.acquire", property: "ttl_seconds"},
		{tool: "leases.renew", property: "ttl_seconds"},
		{tool: "sandboxes.exec", property: "timeout_seconds"},
	}
	for _, testCase := range cases {
		schema := registeredTools[testCase.tool].Descriptor.InputSchema.Properties[testCase.property]
		if schema.Minimum == nil || *schema.Minimum != 0 || schema.Maximum != nil {
			t.Fatalf("%s.%s schema still has a policy ceiling: %#v", testCase.tool, testCase.property, schema)
		}
	}
}

func TestResolveSecretFileAndPermissions(t *testing.T) {
	secretPath := filepath.Join(t.TempDir(), "admin.token")
	if err := os.WriteFile(secretPath, []byte("file-token-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	secret, err := resolveSecret("", secretPath, "admin")
	if err != nil || secret != "file-token-value" {
		t.Fatalf("resolve secret = %q, %v", secret, err)
	}
	if _, err := resolveSecret("direct-token", secretPath, "admin"); err == nil {
		t.Fatal("expected direct value and file conflict")
	}
	if err := os.Chmod(secretPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveSecret("", secretPath, "admin"); err == nil || !strings.Contains(err.Error(), "permissions are too broad") {
		t.Fatalf("expected broad permission rejection, got %v", err)
	}
	emptyPath := filepath.Join(t.TempDir(), "empty.token")
	if err := os.WriteFile(emptyPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveSecret("", emptyPath, "admin"); err == nil || !strings.Contains(err.Error(), "token file is empty") {
		t.Fatalf("expected empty token file rejection, got %v", err)
	}
	symlinkPath := filepath.Join(t.TempDir(), "linked.token")
	if err := os.Symlink(emptyPath, symlinkPath); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveSecret("", symlinkPath, "admin"); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("expected symlink rejection, got %v", err)
	}
}

func TestScopedToolReadsTokenFileWithoutAdminBearer(t *testing.T) {
	disableClientProfile(t)
	t.Setenv("AGENTSPOOL_ADMIN_TOKEN", "")
	t.Setenv("AGENTSPOOL_ADMIN_TOKEN_FILE", "")
	t.Setenv("AGENTSPOOL_RESOURCE_LEASE_TOKEN", "")
	t.Setenv("AGENTSPOOL_RESOURCE_LEASE_TOKEN_FILE", "")
	t.Setenv("AGENTSPOOL_SANDBOX_TOKEN", "")
	t.Setenv("AGENTSPOOL_SANDBOX_TOKEN_FILE", "")
	secretPath := filepath.Join(t.TempDir(), "lease.token")
	if err := os.WriteFile(secretPath, []byte("scoped-token-from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	received := make(chan [2]string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- [2]string{r.Header.Get("Authorization"), r.Header.Get("X-AgentsPool-Resource-Lease-Token")}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"lease":{"id":"lease_test"}}`)
	}))
	defer server.Close()

	err := runTool([]string{
		"leases.get", "--server", server.URL,
		"--admin-token-file", filepath.Join(t.TempDir(), "must-not-be-read"),
		"--resource-lease-token-file", secretPath,
		"--input", `{"lease_id":"lease_test"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	headers := <-received
	if headers[0] != "" || headers[1] != "scoped-token-from-file" {
		t.Fatalf("unexpected scoped headers: authorization=%q resource_lease=%q", headers[0], headers[1])
	}
}

func TestConfigureWritesPrivateDiscoverableClientProfile(t *testing.T) {
	profilePath := filepath.Join(t.TempDir(), "config", "client.json")
	t.Setenv("AGENTSPOOL_CONFIG", profilePath)
	allocatorPath := filepath.Join(t.TempDir(), "allocator.token")
	if err := os.WriteFile(allocatorPath, []byte("allocator-token-from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runConfigure([]string{
		"--server", "https://pool.example.test:8090/",
		"--allocator-token-file", allocatorPath,
	}); err != nil {
		t.Fatal(err)
	}
	profile, loadedPath, err := loadClientProfile()
	if err != nil {
		t.Fatal(err)
	}
	if loadedPath != profilePath || profile.Server != "https://pool.example.test:8090" || profile.AllocatorTokenFile != allocatorPath || profile.AdminTokenFile != "" {
		t.Fatalf("unexpected client profile: path=%q profile=%#v", loadedPath, profile)
	}
	info, err := os.Stat(profilePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("client profile mode=%o, want 0600", info.Mode().Perm())
	}
	if err := os.Chmod(profilePath, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadClientProfile(); err == nil || !strings.Contains(err.Error(), "private regular file") {
		t.Fatalf("expected group-readable profile rejection, got %v", err)
	}
}

func TestAllocatorToolPrefersProfileCredentialWithoutReadingAdmin(t *testing.T) {
	profilePath := filepath.Join(t.TempDir(), "client.json")
	t.Setenv("AGENTSPOOL_CONFIG", profilePath)
	t.Setenv("AGENTSPOOL_SERVER", "")
	t.Setenv("AGENTSPOOL_ADMIN_TOKEN", "")
	t.Setenv("AGENTSPOOL_ADMIN_TOKEN_FILE", "")
	t.Setenv("AGENTSPOOL_ALLOCATOR_TOKEN", "")
	t.Setenv("AGENTSPOOL_ALLOCATOR_TOKEN_FILE", "")
	allocatorPath := filepath.Join(t.TempDir(), "allocator.token")
	if err := os.WriteFile(allocatorPath, []byte("allocator-token-from-profile\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	received := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"revision":1,"devices":[]}`)
	}))
	defer server.Close()
	profile := clientProfile{
		Server: server.URL, AllocatorTokenFile: allocatorPath,
		AdminTokenFile: filepath.Join(t.TempDir(), "admin-must-not-be-read.token"),
	}
	encoded, err := json.Marshal(profile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(profilePath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runTool([]string{"devices.list", "--input", `{}`}); err != nil {
		t.Fatal(err)
	}
	if got := <-received; got != "Bearer allocator-token-from-profile" {
		t.Fatalf("unexpected allocator Authorization header: %q", got)
	}
}
