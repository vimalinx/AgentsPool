package worker

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vimalinx/AgentsPool/internal/client"
)

func TestArtifactCacheConcurrentMaterializeDownloadsOnce(t *testing.T) {
	const artifactID = "art_concurrent_test"
	payload := []byte("shared cached payload\n")
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/artifacts/"+artifactID+"/content" {
			http.NotFound(w, r)
			return
		}
		requests.Add(1)
		_, _ = w.Write(payload)
	}))
	defer server.Close()

	api, err := client.New(server.URL, "node-token", 2*time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	cache, err := newArtifactCache(filepath.Join(t.TempDir(), "cache"), 1<<20, api)
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}

	const parallel = 16
	var wg sync.WaitGroup
	errors := make(chan error, parallel)
	for i := 0; i < parallel; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			destination := filepath.Join(workspace, fmt.Sprintf("input-%02d.txt", index))
			if err := cache.Materialize(context.Background(), artifactID, destination, false); err != nil {
				errors <- err
				return
			}
			data, err := os.ReadFile(destination)
			if err != nil {
				errors <- err
				return
			}
			if string(data) != string(payload) {
				errors <- fmt.Errorf("destination %d content mismatch: %q", index, data)
			}
		}(i)
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("artifact was downloaded %d times, want 1", got)
	}
	ids := cache.IDs(10)
	if len(ids) != 1 || ids[0] != artifactID {
		t.Fatalf("unexpected cache ids: %#v", ids)
	}
}
