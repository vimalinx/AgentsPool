package logstream

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestDiskSegmentsSurviveReopenAndDeduplicateRetries(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	data := bytes.Repeat([]byte("a"), int(2*SegmentBytes+17))
	end, err := s.AppendAt("sbxop_test", "stdout", 0, data, false)
	if err != nil || end != int64(len(data)) {
		t.Fatalf("append: end=%d err=%v", end, err)
	}
	if got, err := s.AppendAt("sbxop_test", "stdout", 0, data, false); err != nil || got != end {
		t.Fatalf("duplicate: %d %v", got, err)
	}
	reopened := &Store{Dir: s.Dir}
	var read []byte
	after := int64(0)
	for after < end {
		page, err := reopened.Read("sbxop_test", "stdout", after, PageBytes)
		if err != nil {
			t.Fatal(err)
		}
		if page.NextAfter <= after {
			t.Fatal("cursor stopped advancing")
		}
		read = append(read, page.Data...)
		after = page.NextAfter
	}
	if !bytes.Equal(read, data) {
		t.Fatal("log bytes changed across disk segments")
	}
	tail, truncated, err := reopened.Tail("sbxop_test", "stdout", 100)
	if err != nil || !truncated || len(tail) != 100 {
		t.Fatalf("tail: len=%d truncated=%v err=%v", len(tail), truncated, err)
	}
}
func TestOptInRotationReportsGap(t *testing.T) {
	s := &Store{Dir: t.TempDir(), RetainBytes: 16}
	if _, err := s.AppendAt("sbxop_test", "stderr", 0, bytes.Repeat([]byte("z"), 80), false); err != nil {
		t.Fatal(err)
	}
	page, err := s.Read("sbxop_test", "stderr", 0, 64)
	if err != nil || !page.Truncated || page.FirstOffset == 0 || page.NextAfter != 80 {
		t.Fatalf("rotation page=%+v err=%v", page, err)
	}
	mirror := &Store{Dir: t.TempDir()}
	if _, err := mirror.AppendAt("sbxop_test", "stderr", page.Offset, page.Data, false); err == nil {
		t.Fatal("unannounced gap accepted")
	}
	if _, err := mirror.AppendAt("sbxop_test", "stderr", page.Offset, page.Data, true); err != nil {
		t.Fatal(err)
	}
}
func TestLogPathsRejectTraversal(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	for _, id := range []string{"../outside", "/absolute", "a/b"} {
		if _, err := s.AppendAt(id, "stdout", 0, []byte("secret"), false); err == nil {
			t.Fatalf("accepted %q", id)
		}
	}
	if _, err := s.Read("sbxop_test", "../../outside", 0, 10); err == nil {
		t.Fatal("invalid stream accepted")
	}
}

func TestRotationBoundsDiskAcrossLargeAndSmallAppends(t *testing.T) {
	s := &Store{Dir: t.TempDir(), RetainBytes: 16}
	want := bytes.Repeat([]byte("x"), int(3*SegmentBytes+7))
	if _, err := s.AppendAt("sbxop_rotation", "stdout", 0, want, false); err != nil {
		t.Fatal(err)
	}
	page, err := s.Read("sbxop_rotation", "stdout", 0, PageBytes)
	if err != nil || len(page.Data) != 16 || !page.Truncated {
		t.Fatalf("rotated page %+v: %v", page, err)
	}
	// The payload retained on disk is at most the requested window plus segment
	// granularity; old full segments must disappear even within one large Write.
	files, err := filepath.Glob(filepath.Join(s.Dir, "sbxop_rotation", "*.log"))
	if err != nil {
		t.Fatal(err)
	}
	var size int64
	for _, path := range files {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		size += info.Size()
	}
	if size > s.RetainBytes+2*SegmentBytes {
		t.Fatalf("rotation retained %d bytes", size)
	}
}
