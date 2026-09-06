package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/vimalinx/AgentsPool/internal/core"
)

func TestOpenMigratesSchemaOneState(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "artifacts"), 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := map[string]any{
		"schema_version": 1, "nodes": map[string]any{}, "tasks": map[string]any{},
		"leases": map[string]any{}, "executions": map[string]any{}, "artifacts": map[string]any{},
		"idempotency": map[string]any{}, "events": []any{},
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	stateStore, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := stateStore.View(func(state *core.State) error {
		if state.SchemaVersion != core.StateSchemaVersion || state.ResourceLeases == nil || state.Sandboxes == nil || state.SandboxOperations == nil {
			t.Fatalf("legacy state was not migrated: %#v", state)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestOpenMigratesSchemaTwoDevicesWithoutDescriptions(t *testing.T) {
	dir := t.TempDir()
	legacy := map[string]any{
		"schema_version": 2,
		"nodes": map[string]any{
			"node_legacy": map[string]any{
				"id": "node_legacy", "machine_id": "machine", "name": "legacy", "status": core.NodeOffline,
				"capabilities": map[string]any{
					"hostname": "legacy", "os": "linux", "arch": "amd64", "cpu_threads": 2,
					"memory_total_mb": 1024, "disk_total_mb": 4096, "capabilities": []any{}, "agent_version": "0.3",
				},
				"resources": map[string]any{"max_concurrency": 1},
			},
		},
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	stateStore, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := stateStore.View(func(state *core.State) error {
		if state.SchemaVersion != core.StateSchemaVersion || state.Nodes["node_legacy"] == nil {
			t.Fatalf("schema-two state was not migrated: %#v", state)
		}
		if len(state.Nodes["node_legacy"].Descriptions) != 0 {
			t.Fatalf("legacy node gained unexpected descriptions: %#v", state.Nodes["node_legacy"].Descriptions)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestOpenMigratesSchemaThreeSandboxOperations(t *testing.T) {
	dir := t.TempDir()
	legacy := map[string]any{
		"schema_version": 3,
		"sandbox_operations": map[string]any{
			"sbxop_legacy": map[string]any{
				"id": "sbxop_legacy", "sandbox_id": "sbx_legacy", "node_id": "node_legacy",
				"kind": "exec", "status": core.SandboxOperationSucceeded,
			},
		},
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	stateStore, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := stateStore.View(func(state *core.State) error {
		if state.SchemaVersion != core.StateSchemaVersion || state.SandboxOperations["sbxop_legacy"] == nil {
			t.Fatalf("schema-three state was not migrated: %#v", state)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestOpenRepairsPrivatePermissions(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"logs", "artifacts"} {
		path := filepath.Join(dir, name)
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	state := core.NewState()
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, "state.json")
	if err := os.WriteFile(statePath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{dir, filepath.Join(dir, "logs"), filepath.Join(dir, "artifacts")} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Fatalf("%s mode = %o, want 0700", path, info.Mode().Perm())
		}
	}
	info, err := os.Stat(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("state mode = %o, want 0600", info.Mode().Perm())
	}
}

func TestStorePersistsDurableUpdates(t *testing.T) {
	dir := t.TempDir()
	first, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Update(func(state *core.State) error {
		state.Tasks["task_one"] = &core.Task{ID: "task_one", Status: core.TaskQueued}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	second, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.View(func(state *core.State) error {
		if state.Tasks["task_one"] == nil {
			t.Fatalf("durable task was not restored")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestVolatileUpdateWaitsForFlush(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	current, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := current.UpdateVolatile(func(state *core.State) error {
		state.Nodes["node_volatile"] = &core.Node{ID: "node_volatile"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("volatile update unexpectedly wrote state.json")
	}

	notFlushed, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = notFlushed.View(func(state *core.State) error {
		if state.Nodes["node_volatile"] != nil {
			t.Fatalf("volatile state should not survive before Flush")
		}
		return nil
	})

	if err := current.Flush(); err != nil {
		t.Fatal(err)
	}
	flushed, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = flushed.View(func(state *core.State) error {
		if state.Nodes["node_volatile"] == nil {
			t.Fatalf("flushed volatile state was not restored")
		}
		return nil
	})
}

func TestDurableUpdateRollsBackCallbackMutationOnError(t *testing.T) {
	stateStore, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	original := &core.Task{ID: "task_original", Status: core.TaskQueued}
	if err := stateStore.Update(func(state *core.State) error {
		state.Tasks[original.ID] = original
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	expectedErr := errors.New("reject update")
	err = stateStore.Update(func(state *core.State) error {
		state.Tasks[original.ID].Status = core.TaskRunning
		state.Tasks["task_partial"] = &core.Task{ID: "task_partial", Status: core.TaskQueued}
		return expectedErr
	})
	if !errors.Is(err, expectedErr) {
		t.Fatalf("got %v, want %v", err, expectedErr)
	}
	if err := stateStore.View(func(state *core.State) error {
		if state.Tasks[original.ID].Status != core.TaskQueued {
			t.Fatalf("original task mutation leaked after error: %#v", state.Tasks[original.ID])
		}
		if state.Tasks["task_partial"] != nil {
			t.Fatal("partially inserted task leaked after error")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(stateStore.DataDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.View(func(state *core.State) error {
		if state.Tasks[original.ID].Status != core.TaskQueued || state.Tasks["task_partial"] != nil {
			t.Fatalf("disk state changed after rejected update: %#v", state.Tasks)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
