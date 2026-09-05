package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/qiyuehuajin/agentspool/internal/core"
)

type Store struct {
	mu      sync.RWMutex
	state   core.State
	path    string
	dataDir string
	logsDir string
	artDir  string
}

func Open(dataDir string) (*Store, error) {
	if dataDir == "" {
		return nil, errors.New("data directory is required")
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	if err := os.Chmod(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("protect data directory: %w", err)
	}
	logsDir := filepath.Join(dataDir, "logs")
	artDir := filepath.Join(dataDir, "artifacts")
	if err := os.MkdirAll(logsDir, 0o700); err != nil {
		return nil, fmt.Errorf("create logs directory: %w", err)
	}
	if err := os.Chmod(logsDir, 0o700); err != nil {
		return nil, fmt.Errorf("protect logs directory: %w", err)
	}
	if err := os.MkdirAll(artDir, 0o700); err != nil {
		return nil, fmt.Errorf("create artifacts directory: %w", err)
	}
	if err := os.Chmod(artDir, 0o700); err != nil {
		return nil, fmt.Errorf("protect artifacts directory: %w", err)
	}

	s := &Store{
		state:   core.NewState(),
		path:    filepath.Join(dataDir, "state.json"),
		dataDir: dataDir,
		logsDir: logsDir,
		artDir:  artDir,
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) DataDir() string      { return s.dataDir }
func (s *Store) LogsDir() string      { return s.logsDir }
func (s *Store) ArtifactsDir() string { return s.artDir }

func (s *Store) load() error {
	if err := os.Chmod(s.path, 0o600); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("protect state: %w", err)
	}
	data, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return s.saveLocked()
	}
	if err != nil {
		return fmt.Errorf("read state: %w", err)
	}
	var state core.State
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("decode state: %w", err)
	}
	state.EnsureMaps()
	if state.SchemaVersion > core.StateSchemaVersion {
		return fmt.Errorf("state schema %d is newer than supported schema %d", state.SchemaVersion, core.StateSchemaVersion)
	}
	needsMigration := state.SchemaVersion < core.StateSchemaVersion
	if needsMigration {
		state.SchemaVersion = core.StateSchemaVersion
	}
	s.state = state
	if needsMigration {
		return s.saveLocked()
	}
	return nil
}

func (s *Store) View(fn func(*core.State) error) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return fn(&s.state)
}

func (s *Store) Update(fn func(*core.State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	before, err := json.Marshal(s.state)
	if err != nil {
		return fmt.Errorf("snapshot state before update: %w", err)
	}
	if err := fn(&s.state); err != nil {
		if rollbackErr := s.restoreLocked(before); rollbackErr != nil {
			return fmt.Errorf("update failed: %w; restore previous state: %v", err, rollbackErr)
		}
		return err
	}
	if err := s.saveLocked(); err != nil {
		if rollbackErr := s.restoreLocked(before); rollbackErr != nil {
			return fmt.Errorf("persist update: %w; restore previous state: %v", err, rollbackErr)
		}
		return err
	}
	return nil
}

// UpdateVolatile mutates in-memory state without forcing a disk snapshot. It is
// intended for high-frequency, reconstructable observations such as worker
// heartbeats and lease renewals. The mutation is still serialized with durable
// updates, and a later durable update or Flush persists the latest state.
func (s *Store) UpdateVolatile(fn func(*core.State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fn(&s.state)
}

func (s *Store) Snapshot() (core.State, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	data, err := json.Marshal(s.state)
	if err != nil {
		return core.State{}, fmt.Errorf("encode state snapshot: %w", err)
	}
	var out core.State
	if err := json.Unmarshal(data, &out); err != nil {
		return core.State{}, fmt.Errorf("decode state snapshot: %w", err)
	}
	out.EnsureMaps()
	return out, nil
}

func (s *Store) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked()
}

func (s *Store) restoreLocked(encoded []byte) error {
	var restored core.State
	if err := json.Unmarshal(encoded, &restored); err != nil {
		return err
	}
	restored.EnsureMaps()
	s.state = restored
	return nil
}

func (s *Store) saveLocked() error {
	data, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	tmp, err := os.CreateTemp(s.dataDir, ".state-*.tmp")
	if err != nil {
		return fmt.Errorf("create state temp file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return fmt.Errorf("chmod state temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("write state temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("sync state temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close state temp file: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("replace state file: %w", err)
	}
	if dir, err := os.Open(s.dataDir); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}
