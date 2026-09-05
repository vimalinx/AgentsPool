package store

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type LogEvent struct {
	Sequence  int64     `json:"sequence"`
	Timestamp time.Time `json:"timestamp"`
	Stream    string    `json:"stream"`
	Data      string    `json:"data"`
}

type LogStore struct {
	dir   string
	locks sync.Map
}

func NewLogStore(dir string) *LogStore {
	return &LogStore{dir: dir}
}

func (l *LogStore) lockFor(id string) *sync.Mutex {
	value, _ := l.locks.LoadOrStore(id, &sync.Mutex{})
	return value.(*sync.Mutex)
}

func (l *LogStore) path(executionID string) string {
	return filepath.Join(l.dir, executionID+".jsonl")
}

func (l *LogStore) Append(executionID string, events []LogEvent) error {
	if len(events) == 0 {
		return nil
	}
	lock := l.lockFor(executionID)
	lock.Lock()
	defer lock.Unlock()

	file, err := os.OpenFile(l.path(executionID), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open execution log: %w", err)
	}
	defer file.Close()
	writer := bufio.NewWriter(file)
	encoder := json.NewEncoder(writer)
	for _, event := range events {
		if err := encoder.Encode(event); err != nil {
			return fmt.Errorf("write execution log: %w", err)
		}
	}
	if err := writer.Flush(); err != nil {
		return fmt.Errorf("flush execution log: %w", err)
	}
	return file.Sync()
}

func (l *LogStore) Read(executionID string, after int64, limit int) ([]LogEvent, error) {
	if limit <= 0 || limit > 5000 {
		limit = 1000
	}
	lock := l.lockFor(executionID)
	lock.Lock()
	defer lock.Unlock()

	file, err := os.Open(l.path(executionID))
	if errors.Is(err, os.ErrNotExist) {
		return []LogEvent{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open execution log: %w", err)
	}
	defer file.Close()

	decoder := json.NewDecoder(bufio.NewReader(file))
	out := make([]LogEvent, 0, limit)
	for {
		var event LogEvent
		if err := decoder.Decode(&event); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("decode execution log: %w", err)
		}
		if event.Sequence <= after {
			continue
		}
		out = append(out, event)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (l *LogStore) Delete(executionID string) error {
	lock := l.lockFor(executionID)
	lock.Lock()
	defer lock.Unlock()
	if err := os.Remove(l.path(executionID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	l.locks.Delete(executionID)
	return nil
}
