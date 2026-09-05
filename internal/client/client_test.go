package client

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDownloadVerifiesSHA256ETagAndInstallsAtomically(t *testing.T) {
	payload := []byte("verified artifact\n")
	digest := fmt.Sprintf("%x", sha256.Sum256(payload))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("ETag", `"sha256:`+digest+`"`)
		_, _ = w.Write(payload)
	}))
	defer server.Close()

	api, err := New(server.URL, "", time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "nested", "artifact.txt")
	if err := api.Download(context.Background(), "/artifact", nil, destination, nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(payload) {
		t.Fatalf("downloaded content %q, want %q", data, payload)
	}
}

func TestDownloadRejectsDigestMismatchWithoutReplacingDestination(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("ETag", `"sha256:`+strings.Repeat("0", 64)+`"`)
		_, _ = w.Write([]byte("tampered"))
	}))
	defer server.Close()

	api, err := New(server.URL, "", time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "artifact.txt")
	if err := os.WriteFile(destination, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = api.Download(context.Background(), "/artifact", nil, destination, nil)
	if err == nil || !strings.Contains(err.Error(), "integrity check failed") {
		t.Fatalf("expected integrity error, got %v", err)
	}
	data, readErr := os.ReadFile(destination)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(data) != "original" {
		t.Fatalf("destination was replaced after failed verification: %q", data)
	}
}

func TestSHA256FromETag(t *testing.T) {
	digest := strings.Repeat("a", 64)
	if got := sha256FromETag(`"sha256:` + digest + `"`); got != digest {
		t.Fatalf("got %q", got)
	}
	for _, value := range []string{"", `W/"sha256:` + digest + `"`, `"md5:abc"`, `"sha256:short"`} {
		if got := sha256FromETag(value); got != "" {
			t.Fatalf("expected %q to be ignored, got %q", value, got)
		}
	}
}

func TestExplicitContextDeadlineOverridesClientDefaultTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(75 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	api, err := New(server.URL, "", 20*time.Millisecond, false)
	if err != nil {
		t.Fatal(err)
	}
	var timedOut map[string]bool
	if err := api.DoJSON(context.Background(), http.MethodGet, "/slow", nil, nil, &timedOut, nil); err == nil {
		t.Fatal("default client timeout was not applied")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	var response map[string]bool
	if err := api.DoJSON(ctx, http.MethodGet, "/slow", nil, nil, &response, nil); err != nil {
		t.Fatalf("explicit longer deadline should override default timeout: %v", err)
	}
	if !response["ok"] {
		t.Fatalf("unexpected response: %#v", response)
	}
}

func TestZeroClientTimeoutMeansNoDefaultDeadline(t *testing.T) {
	api, err := New("http://127.0.0.1:8090", "", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if api.DefaultTimeout != 0 {
		t.Fatalf("default timeout = %s, want no deadline", api.DefaultTimeout)
	}
	if _, err := New("http://127.0.0.1:8090", "", -time.Second, false); err == nil {
		t.Fatal("negative timeout must be rejected")
	}
}
