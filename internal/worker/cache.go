package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/qiyuehuajin/agentspool/internal/client"
)

type artifactCache struct {
	mu       sync.Mutex
	dir      string
	maxBytes int64
	client   *client.Client
}

func newArtifactCache(dir string, maxBytes int64, apiClient *client.Client) (*artifactCache, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	return &artifactCache{dir: dir, maxBytes: maxBytes, client: apiClient}, nil
}

func (c *artifactCache) IDs(limit int) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return nil
	}
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !validArtifactID(entry.Name()) {
			continue
		}
		ids = append(ids, entry.Name())
		if limit > 0 && len(ids) >= limit {
			break
		}
	}
	sort.Strings(ids)
	return ids
}

func (c *artifactCache) Materialize(ctx context.Context, artifactID, destination string, executable bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !validArtifactID(artifactID) {
		return fmt.Errorf("invalid artifact id %q", artifactID)
	}
	cached := filepath.Join(c.dir, artifactID)
	if _, err := os.Stat(cached); errors.Is(err, os.ErrNotExist) {
		tmp, err := os.CreateTemp(c.dir, ".download-*.tmp")
		if err != nil {
			return err
		}
		tmpPath := tmp.Name()
		_ = tmp.Close()
		if err := c.client.Download(ctx, "/api/v1/artifacts/"+artifactID+"/content", nil, tmpPath, nil); err != nil {
			_ = os.Remove(tmpPath)
			return fmt.Errorf("download artifact %s: %w", artifactID, err)
		}
		if err := os.Chmod(tmpPath, 0o600); err != nil {
			_ = os.Remove(tmpPath)
			return err
		}
		if err := os.Rename(tmpPath, cached); err != nil {
			if _, statErr := os.Stat(cached); statErr == nil {
				_ = os.Remove(tmpPath)
			} else {
				_ = os.Remove(tmpPath)
				return err
			}
		}
	}
	now := time.Now()
	_ = os.Chtimes(cached, now, now)
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	if err := linkOrCopy(cached, destination); err != nil {
		return err
	}
	mode := os.FileMode(0o600)
	if executable {
		mode = 0o700
	}
	if err := os.Chmod(destination, mode); err != nil {
		return err
	}
	return c.evictLocked()
}

func (c *artifactCache) evictLocked() error {
	if c.maxBytes <= 0 {
		return nil
	}
	type item struct {
		path    string
		size    int64
		modTime time.Time
	}
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return err
	}
	items := []item{}
	var total int64
	for _, entry := range entries {
		if entry.IsDir() || !validArtifactID(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		items = append(items, item{path: filepath.Join(c.dir, entry.Name()), size: info.Size(), modTime: info.ModTime()})
		total += info.Size()
	}
	if total <= c.maxBytes {
		return nil
	}
	sort.Slice(items, func(i, j int) bool { return items[i].modTime.Before(items[j].modTime) })
	for _, item := range items {
		if total <= c.maxBytes {
			break
		}
		if err := os.Remove(item.path); err == nil {
			total -= item.size
		}
	}
	return nil
}

func linkOrCopy(source, destination string) error {
	// Never hard-link a cached artifact into a mutable job workspace: a task
	// could otherwise modify the shared cache inode and poison later jobs.
	_ = os.Remove(destination)
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		_ = output.Close()
		if !ok {
			_ = os.Remove(destination)
		}
	}()
	if _, err := io.Copy(output, input); err != nil {
		return err
	}
	if err := output.Sync(); err != nil {
		return err
	}
	ok = true
	return nil
}

func validArtifactID(value string) bool {
	if !strings.HasPrefix(value, "art_") || len(value) > 128 {
		return false
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '_' || char == '-' {
			continue
		}
		return false
	}
	return true
}
