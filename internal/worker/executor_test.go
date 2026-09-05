package worker

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiyuehuajin/agentspool/internal/config"
	"github.com/qiyuehuajin/agentspool/internal/core"
)

func TestNativeExecutorRunsAndCapturesOutput(t *testing.T) {
	workspace := t.TempDir()
	cfg := config.DefaultWorker()
	cfg.AllowedExecutors = map[string]bool{core.ExecutorShell: true}
	cfg.ShellBinary = "/bin/sh"
	var output strings.Builder
	result := executeTask(context.Background(), cfg, "exec_test", core.ExecutorSpec{
		Kind:   core.ExecutorShell,
		Script: "printf 'hello:%s' \"$TASK_VALUE\"; mkdir -p outputs; printf done > outputs/result.txt",
		Env:    map[string]string{"TASK_VALUE": "world"},
	}, workspace, nil, func(_ string, data string) { output.WriteString(data) }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if result.Err != nil || result.ExitCode != 0 {
		t.Fatalf("execution failed: %#v", result)
	}
	if output.String() != "hello:world" {
		t.Fatalf("unexpected output %q", output.String())
	}
	data, err := os.ReadFile(filepath.Join(workspace, "outputs", "result.txt"))
	if err != nil || string(data) != "done" {
		t.Fatalf("output file mismatch: %q, %v", data, err)
	}
}

func TestNativeExecutorCancellation(t *testing.T) {
	workspace := t.TempDir()
	cfg := config.DefaultWorker()
	cfg.AllowedExecutors = map[string]bool{core.ExecutorShell: true}
	cfg.ShellBinary = "/bin/sh"
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	result := executeTask(ctx, cfg, "exec_cancel", core.ExecutorSpec{Kind: core.ExecutorShell, Script: "sleep 30"}, workspace, nil, func(_, _ string) {}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if !result.Cancelled || result.Err == nil {
		t.Fatalf("expected cancelled execution, got %#v", result)
	}
}

func TestHostEnvironmentIsSanitizedByDefault(t *testing.T) {
	t.Setenv("AGENTSPOOL_JOIN_TOKEN", "must-not-leak")
	t.Setenv("UNRELATED_API_SECRET", "must-not-leak-either")
	t.Setenv("PATH", "/usr/bin:/bin")
	env := mergeHostEnvironment(map[string]string{"EXPLICIT": "allowed"}, false)
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "must-not-leak") {
		t.Fatalf("sensitive host environment leaked: %s", joined)
	}
	if !strings.Contains(joined, "PATH=/usr/bin:/bin") || !strings.Contains(joined, "EXPLICIT=allowed") {
		t.Fatalf("safe or explicit environment missing: %s", joined)
	}
	env = mergeHostEnvironment(nil, true)
	if !strings.Contains(strings.Join(env, "\n"), "UNRELATED_API_SECRET=must-not-leak-either") {
		t.Fatal("inherit-all mode did not preserve host environment")
	}
}

func TestSecretReferences(t *testing.T) {
	resolved, err := resolveEnvironment(map[string]string{"API_KEY": "${secret:MODEL_KEY}"}, map[string]string{"MODEL_KEY": "secret-value"})
	if err != nil || resolved["API_KEY"] != "secret-value" {
		t.Fatalf("secret resolution failed: %#v %v", resolved, err)
	}
	if _, err := resolveEnvironment(map[string]string{"API_KEY": "${secret:MISSING}"}, nil); err == nil {
		t.Fatal("missing secret should fail")
	}
}

func TestDockerSecretsUsePrivateEnvFileInsteadOfCommandArguments(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	argsLog := filepath.Join(root, "docker-args.log")
	fakeDocker := filepath.Join(root, "docker")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> \"$ARGS_LOG\"\n"
	if err := os.WriteFile(fakeDocker, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARGS_LOG", argsLog)

	cfg := config.DefaultWorker()
	cfg.AllowedExecutors = map[string]bool{core.ExecutorDocker: true}
	cfg.DockerBinary = fakeDocker
	result := executeTask(context.Background(), cfg, "exec_secret", core.ExecutorSpec{
		Kind:    core.ExecutorDocker,
		Image:   "example.invalid/test:latest",
		Network: "none",
		Env:     map[string]string{"API_KEY": "super-secret-value"},
	}, workspace, nil, func(_, _ string) {}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if result.Err != nil || result.ExitCode != 0 {
		t.Fatalf("fake Docker execution failed: %#v", result)
	}
	data, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatal(err)
	}
	arguments := string(data)
	if strings.Contains(arguments, "super-secret-value") {
		t.Fatalf("secret leaked into Docker command arguments: %s", arguments)
	}
	if !strings.Contains(arguments, "--env-file") {
		t.Fatalf("Docker command did not use --env-file: %s", arguments)
	}
	matches, err := filepath.Glob(filepath.Join(root, ".agentspool-docker-env-*.list"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary Docker env file was not removed: %v", matches)
	}
}

func TestWriteDockerEnvFileRejectsMultilineValues(t *testing.T) {
	if _, err := writeDockerEnvFile(t.TempDir(), map[string]string{"PRIVATE_KEY": "line1\nline2"}); err == nil {
		t.Fatal("expected multiline Docker environment value to be rejected")
	}
}
