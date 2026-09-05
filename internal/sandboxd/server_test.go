package sandboxd

import (
	"bytes"
	"testing"
)

func TestOperationOutputPersistsBeyondResultPreview(t *testing.T) {
	s := &Server{cfg: Config{DataDir: t.TempDir()}}
	logs := s.operationLogs("sbx_test")
	want := bytes.Repeat([]byte("output\n"), 20000)
	if _, err := logs.Writer("sbxop_test", "stdout").Write(want); err != nil {
		t.Fatal(err)
	}
	tail, truncated, err := logs.Tail("sbxop_test", "stdout", 65536)
	if err != nil || !truncated || len(tail) != 65536 {
		t.Fatalf("preview size=%d truncated=%v err=%v", len(tail), truncated, err)
	}
	var got []byte
	for offset := int64(0); offset < int64(len(want)); {
		page, err := logs.Read("sbxop_test", "stdout", offset, 65536)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, page.Data...)
		offset = page.NextAfter
	}
	if !bytes.Equal(got, want) {
		t.Fatal("full output differs from source")
	}
}
