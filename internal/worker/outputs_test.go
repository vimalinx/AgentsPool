package worker

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/qiyuehuajin/agentspool/internal/core"
)

func TestCollectOutputFilesRecursiveGlob(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "nested", "deep"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{
		"root.txt": "root", "nested/deep/result.txt": "nested", "nested/ignore.bin": "bin",
	} {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	files, err := collectOutputFiles(root, []core.OutputRule{{Pattern: "**/*.txt", Required: true}}, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0].RelativePath != "nested/deep/result.txt" || files[1].RelativePath != "root.txt" {
		t.Fatalf("unexpected matches: %#v", files)
	}
}

func TestCollectOutputFilesRequiredAndLimit(t *testing.T) {
	root := t.TempDir()
	if _, err := collectOutputFiles(root, []core.OutputRule{{Pattern: "missing.txt", Required: true}}, 1024); err == nil {
		t.Fatal("required missing output should fail")
	}
	if err := os.WriteFile(filepath.Join(root, "big.txt"), make([]byte, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := collectOutputFiles(root, []core.OutputRule{{Pattern: "*.txt"}}, 8); err == nil {
		t.Fatal("output byte limit should fail")
	}
}

func TestCompileGlob(t *testing.T) {
	tests := []struct {
		pattern string
		match   string
		want    bool
	}{
		{"**/*.json", "result.json", true},
		{"**/*.json", "a/b/result.json", true},
		{"outputs/*.txt", "outputs/a.txt", true},
		{"outputs/*.txt", "outputs/deep/a.txt", false},
	}
	for _, test := range tests {
		re, err := compileGlob(test.pattern)
		if err != nil {
			t.Fatalf("compile %s: %v", test.pattern, err)
		}
		if got := re.MatchString(test.match); got != test.want {
			t.Fatalf("pattern %q match %q = %v, want %v", test.pattern, test.match, got, test.want)
		}
	}
}

func TestCollectOutputFilesCountsOverlappingRequiredRules(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "outputs", "result.txt")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("result"), 0o600); err != nil {
		t.Fatal(err)
	}

	files, err := collectOutputFiles(workspace, []core.OutputRule{
		{Pattern: "outputs/**/*.txt", Required: true, MediaType: "text/plain"},
		{Pattern: "outputs/result.*", Required: true},
	}, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].RelativePath != "outputs/result.txt" {
		t.Fatalf("unexpected outputs: %#v", files)
	}
	if files[0].MediaType != "text/plain" {
		t.Fatalf("first matching rule should select media type, got %q", files[0].MediaType)
	}
}
