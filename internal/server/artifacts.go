package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/qiyuehuajin/agentspool/internal/core"
)

func (s *Server) ingestArtifact(w http.ResponseWriter, r *http.Request, name, mediaType, producerTaskID, producerExecutionID string, metadata map[string]string) (*core.Artifact, error) {
	s.artifactMu.Lock()
	defer s.artifactMu.Unlock()

	if s.cfg.MaxArtifactBytes > 0 && r.ContentLength > s.cfg.MaxArtifactBytes && r.ContentLength >= 0 {
		return nil, fmt.Errorf("artifact exceeds maximum size of %d bytes", s.cfg.MaxArtifactBytes)
	}
	name = sanitizeFilename(name)
	if mediaType == "" {
		mediaType = strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0])
	}
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}

	tmp, err := os.CreateTemp(s.store.ArtifactsDir(), ".upload-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("create artifact temp file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return nil, err
	}

	hash := sha256.New()
	source := io.Reader(r.Body)
	if s.cfg.MaxArtifactBytes > 0 {
		source = http.MaxBytesReader(w, r.Body, s.cfg.MaxArtifactBytes)
	}
	written, err := io.Copy(io.MultiWriter(tmp, hash), source)
	if err != nil {
		cleanup()
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return nil, fmt.Errorf("artifact exceeds maximum size of %d bytes", s.cfg.MaxArtifactBytes)
		}
		return nil, fmt.Errorf("write artifact: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return nil, fmt.Errorf("sync artifact: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return nil, fmt.Errorf("close artifact: %w", err)
	}

	digest := hex.EncodeToString(hash.Sum(nil))
	artifactID, err := core.NewID("art")
	if err != nil {
		_ = os.Remove(tmpPath)
		return nil, fmt.Errorf("generate artifact id: %w", err)
	}
	relPath := filepath.Join(digest[:2], digest)
	finalPath := filepath.Join(s.store.ArtifactsDir(), relPath)
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o700); err != nil {
		_ = os.Remove(tmpPath)
		return nil, fmt.Errorf("create artifact shard: %w", err)
	}
	if _, err := os.Stat(finalPath); errors.Is(err, os.ErrNotExist) {
		if err := os.Rename(tmpPath, finalPath); err != nil {
			_ = os.Remove(tmpPath)
			return nil, fmt.Errorf("store artifact: %w", err)
		}
	} else if err != nil {
		_ = os.Remove(tmpPath)
		return nil, fmt.Errorf("inspect artifact destination: %w", err)
	} else {
		_ = os.Remove(tmpPath)
	}

	now := time.Now().UTC()
	var result *core.Artifact
	err = s.store.Update(func(state *core.State) error {
		artifact := &core.Artifact{
			ID: artifactID, SHA256: digest, Name: name, MediaType: mediaType, Size: written,
			StoragePath: relPath, CreatedAt: now, ProducerTaskID: producerTaskID,
			ProducerExecutionID: producerExecutionID, Metadata: metadata,
		}
		state.Artifacts[artifactID] = artifact
		addEvent(state, s.cfg.EventRetention, "artifact.created", actorFromRequest(r), "artifact", artifactID, "artifact stored", map[string]string{"name": name, "size": fmt.Sprint(written), "sha256": digest})
		copy := cloneViaJSON(*artifact)
		result = &copy
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.metrics.ArtifactsUploaded.Add(1)
	s.metrics.BytesUploaded.Add(uint64(written))
	return result, nil
}

func artifactAbsolutePath(base string, artifact *core.Artifact) (string, error) {
	baseAbs, err := filepath.Abs(base)
	if err != nil {
		return "", err
	}
	candidate, err := filepath.Abs(filepath.Join(base, artifact.StoragePath))
	if err != nil {
		return "", err
	}
	if candidate != baseAbs && !strings.HasPrefix(candidate, baseAbs+string(filepath.Separator)) {
		return "", errors.New("artifact path escapes storage root")
	}
	return candidate, nil
}
