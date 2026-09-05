package worker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/qiyuehuajin/agentspool/internal/config"
)

func TestExistingCredentialsPermissionsAreRepaired(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")
	stored := credentials{
		ServerURL:    "http://127.0.0.1:8090",
		MachineID:    "machine_test",
		NodeID:       "node_test",
		NodeToken:    "synthetic-node-token-for-test-only",
		RegisteredAt: time.Now().UTC(),
	}
	encoded, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0o644); err != nil {
		t.Fatal(err)
	}
	worker := &Worker{
		cfg: config.Worker{
			ServerURL:      stored.ServerURL,
			DataDir:        dir,
			RequestTimeout: time.Second,
		},
		machineID: stored.MachineID,
	}
	if err := worker.ensureCredentials(context.Background()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("credentials mode = %o, want 0600", info.Mode().Perm())
	}
}

func TestExistingCredentialsUpgradeSameServerToHTTPS(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")
	stored := credentials{
		ServerURL:    "http://192.0.2.10:8090",
		MachineID:    "machine_test",
		NodeID:       "node_test",
		NodeToken:    "synthetic-node-token-for-test-only",
		RegisteredAt: time.Now().UTC(),
	}
	encoded, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	worker := &Worker{
		cfg: config.Worker{
			ServerURL:      "https://192.0.2.10:8090",
			DataDir:        dir,
			RequestTimeout: time.Second,
		},
		machineID: stored.MachineID,
	}
	if err := worker.ensureCredentials(context.Background()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var updated credentials
	if err := json.Unmarshal(data, &updated); err != nil {
		t.Fatal(err)
	}
	if updated.ServerURL != worker.cfg.ServerURL {
		t.Fatalf("stored server URL = %q, want %q", updated.ServerURL, worker.cfg.ServerURL)
	}
	if updated.NodeToken != stored.NodeToken || updated.NodeID != stored.NodeID {
		t.Fatal("HTTPS upgrade changed the node identity")
	}
}

func TestExistingCredentialsRejectServerChangeWithoutJoinToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")
	stored := credentials{
		ServerURL:    "http://192.0.2.10:8090",
		MachineID:    "machine_test",
		NodeID:       "node_test",
		NodeToken:    "synthetic-node-token-for-test-only",
		RegisteredAt: time.Now().UTC(),
	}
	encoded, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	worker := &Worker{
		cfg: config.Worker{
			ServerURL:      "https://192.0.2.11:8090",
			DataDir:        dir,
			RequestTimeout: time.Second,
		},
		machineID: stored.MachineID,
	}
	if err := worker.ensureCredentials(context.Background()); err == nil {
		t.Fatal("server change unexpectedly reused stored credentials")
	}
}
