package config

import (
	"strings"
	"testing"
)

func TestDefaultsAreLocalAndNetworkDenied(t *testing.T) {
	for _, key := range []string{
		"AGENTSPOOL_LISTEN",
		"AGENTSPOOL_WORKER_ALLOWED_EXECUTORS",
		"AGENTSPOOL_WORKER_ALLOW_NETWORK",
	} {
		t.Setenv(key, "")
	}

	server := DefaultServer()
	if server.ListenAddress != "127.0.0.1:8090" {
		t.Fatalf("default listen address = %q, want loopback", server.ListenAddress)
	}

	worker := DefaultWorker()
	if worker.AllowNetwork {
		t.Fatal("worker network access must be denied by default")
	}
	if !worker.AllowedExecutors["shell"] || worker.AllowedExecutors["process"] || worker.AllowedExecutors["docker"] {
		t.Fatalf("default executors = %#v, want shell only", worker.AllowedExecutors)
	}
}

func TestBootstrapCredentialsRejectPlaceholdersAndSharedAuthority(t *testing.T) {
	cfg := DefaultServer()
	cfg.AdminToken = "synthetic-admin-" + strings.Repeat("a", 32)
	cfg.AllocatorToken = "synthetic-allocator-" + strings.Repeat("b", 32)
	cfg.JoinToken = "synthetic-join-" + strings.Repeat("c", 32)
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"admin", "allocator", "join"} {
		copy := cfg
		switch name {
		case "admin":
			copy.AdminToken = "replace-with-a-random-admin-token-at-least-24-characters"
		case "allocator":
			copy.AllocatorToken = "replace-with-a-different-random-allocator-token-at-least-24-characters"
		case "join":
			copy.JoinToken = "replace-with-a-different-random-join-token-at-least-24-characters"
		}
		if err := copy.Validate(); err == nil {
			t.Fatalf("accepted %s example token", name)
		}
	}
	cfg.JoinToken = cfg.AdminToken
	if err := cfg.Validate(); err == nil {
		t.Fatal("shared bootstrap authority accepted")
	}
}
