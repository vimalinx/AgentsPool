// Package logstream stores byte-addressed output in bounded-size disk segments.
// A small durable cursor avoids scanning all historical output on every write.
// Retention is opt-in; offsets disclose any explicitly discarded prefix.
package logstream

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sync"
)

const SegmentBytes int64 = 1 << 20
const PageBytes = 64 << 10

var safeID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

type Store struct {
	Dir         string
	RetainBytes int64
	// Fixed stripes bound lock memory even after millions of pruned operations.
	locks [64]sync.Mutex
}
type Page struct {
	Offset      int64  `json:"offset"`
	NextAfter   int64  `json:"next_after"`
	FirstOffset int64  `json:"first_offset"`
	EndOffset   int64  `json:"end_offset"`
	Data        []byte `json:"data_base64"`
	Truncated   bool   `json:"truncated"`
}
type cursor struct {
	Version int   `json:"version"`
	First   int64 `json:"first"`
	End     int64 `json:"end"`
}

func (s *Store) lockIndex(id, stream string) (int, error) {
	if !safeID.MatchString(id) || (stream != "stdout" && stream != "stderr") {
		return 0, errors.New("invalid log identity or stream")
	}
	hash := uint64(14695981039346656037)
	for _, value := range id + "/" + stream {
		hash ^= uint64(value)
		hash *= 1099511628211
	}
	return int(hash % uint64(len(s.locks))), nil
}
func (s *Store) readCursor(id, stream string) (cursor, error) {
	current := cursor{Version: 1}
	data, err := os.ReadFile(filepath.Join(s.Dir, id, stream+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return current, nil
	}
	if err != nil {
		return current, err
	}
	if err := json.Unmarshal(data, &current); err != nil {
		return current, err
	}
	if current.Version != 1 || current.First < 0 || current.End < current.First {
		return current, errors.New("invalid log cursor")
	}
	return current, nil
}
func (s *Store) saveCursor(id, stream string, current cursor) error {
	dir := filepath.Join(s.Dir, id)
	file, err := os.CreateTemp(dir, ".cursor-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	data, err := json.Marshal(current)
	if err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(file.Name(), filepath.Join(dir, stream+".json")); err != nil {
		return err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
func (s *Store) segmentPath(id, stream string, offset int64) string {
	return filepath.Join(s.Dir, id, fmt.Sprintf("%s-%020d.log", stream, offset/SegmentBytes*SegmentBytes))
}
func (s *Store) removeSegments(id, stream string, first, end int64) error {
	for offset := first / SegmentBytes * SegmentBytes; offset < end; {
		if err := os.Remove(s.segmentPath(id, stream, offset)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if offset > math.MaxInt64-SegmentBytes {
			break
		}
		offset += SegmentBytes
	}
	return nil
}

// AppendAt accepts retransmission of a committed prefix. A gap is permitted
// only when the sender explicitly reports rotated history. Offset -1 appends.
func (s *Store) AppendAt(id, stream string, offset int64, data []byte, gap bool) (int64, error) {
	index, err := s.lockIndex(id, stream)
	if err != nil {
		return 0, err
	}
	s.locks[index].Lock()
	defer s.locks[index].Unlock()
	current, err := s.readCursor(id, stream)
	if err != nil {
		return 0, err
	}
	if offset == -1 {
		offset = current.End
	}
	if offset < 0 || offset > math.MaxInt64-int64(len(data)) {
		return current.End, errors.New("invalid log offset")
	}
	if offset > current.End && !gap {
		return current.End, fmt.Errorf("log offset gap: have %d, got %d", current.End, offset)
	}
	if offset < current.End {
		overlap := current.End - offset
		if overlap >= int64(len(data)) {
			return current.End, nil
		}
		data = data[overlap:]
		offset = current.End
	}
	if len(data) == 0 {
		return current.End, nil
	}
	dir := filepath.Join(s.Dir, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return current.End, err
	}
	old := current
	writtenStart := offset
	if offset > current.End {
		current.First = offset
		current.End = offset
	}
	for len(data) > 0 {
		amount := min(int64(len(data)), SegmentBytes-offset%SegmentBytes)
		file, err := os.OpenFile(s.segmentPath(id, stream, offset), os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return old.End, err
		}
		n, writeErr := file.WriteAt(data[:amount], offset%SegmentBytes)
		if writeErr == nil && int64(n) != amount {
			writeErr = io.ErrShortWrite
		}
		if writeErr == nil {
			writeErr = file.Sync()
		}
		closeErr := file.Close()
		if writeErr != nil {
			return old.End, writeErr
		}
		if closeErr != nil {
			return old.End, closeErr
		}
		offset += amount
		data = data[amount:]
	}
	current.End = offset
	if s.RetainBytes > 0 && current.End-current.First > s.RetainBytes {
		current.First = current.End - s.RetainBytes
	}
	// Publish the cursor before removing old segments. Interrupted retention can
	// leave extra disk files, but never advertises bytes already removed.
	if err := s.saveCursor(id, stream, current); err != nil {
		return old.End, err
	}
	if old.End > old.First {
		removeEnd := min(old.End, current.First/SegmentBytes*SegmentBytes)
		if err := s.removeSegments(id, stream, old.First, removeEnd); err != nil {
			return current.End, err
		}
	}
	// A single large append can itself span and expire multiple segments.
	if err := s.removeSegments(id, stream, writtenStart, current.First/SegmentBytes*SegmentBytes); err != nil {
		return current.End, err
	}
	return current.End, nil
}
func (s *Store) Read(id, stream string, after int64, limit int) (Page, error) {
	var page Page
	if after < 0 {
		return page, errors.New("log offset must be non-negative")
	}
	if limit <= 0 || limit > PageBytes {
		limit = PageBytes
	}
	index, err := s.lockIndex(id, stream)
	if err != nil {
		return page, err
	}
	s.locks[index].Lock()
	defer s.locks[index].Unlock()
	current, err := s.readCursor(id, stream)
	if err != nil {
		return page, err
	}
	page.FirstOffset = current.First
	page.EndOffset = current.End
	if after < current.First {
		after = current.First
		page.Truncated = true
	}
	page.Offset = after
	page.NextAfter = after
	count := min(int64(limit), current.End-after)
	if count <= 0 {
		return page, nil
	}
	page.Data = make([]byte, count)
	read := int64(0)
	for read < count {
		amount := min(count-read, SegmentBytes-after%SegmentBytes)
		file, err := os.Open(s.segmentPath(id, stream, after))
		if err != nil {
			return page, err
		}
		n, readErr := file.ReadAt(page.Data[read:read+amount], after%SegmentBytes)
		closeErr := file.Close()
		if readErr != nil {
			return page, readErr
		}
		if closeErr != nil {
			return page, closeErr
		}
		if int64(n) != amount {
			return page, io.ErrUnexpectedEOF
		}
		after += amount
		read += amount
	}
	page.NextAfter = after
	return page, nil
}
func (s *Store) Tail(id, stream string, limit int) (string, bool, error) {
	if limit <= 0 || limit > PageBytes {
		limit = PageBytes
	}
	page, err := s.Read(id, stream, 0, 1)
	if err != nil {
		return "", false, err
	}
	after := max(page.EndOffset-int64(limit), 0)
	tail, err := s.Read(id, stream, after, limit)
	return string(tail.Data), tail.FirstOffset > 0 || after > 0, err
}
func (s *Store) Writer(id, stream string) io.Writer { return &writer{store: s, id: id, stream: stream} }

type writer struct {
	store      *Store
	id, stream string
}

func (w *writer) Write(data []byte) (int, error) {
	if _, err := w.store.AppendAt(w.id, w.stream, -1, data, false); err != nil {
		return 0, err
	}
	return len(data), nil
}
func (s *Store) Delete(id string) error {
	first, err := s.lockIndex(id, "stdout")
	if err != nil {
		return err
	}
	second, _ := s.lockIndex(id, "stderr")
	if first > second {
		first, second = second, first
	}
	s.locks[first].Lock()
	defer s.locks[first].Unlock()
	if first != second {
		s.locks[second].Lock()
		defer s.locks[second].Unlock()
	}
	return os.RemoveAll(filepath.Join(s.Dir, id))
}
