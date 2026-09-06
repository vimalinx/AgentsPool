package worker

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/vimalinx/AgentsPool/internal/config"
	"github.com/vimalinx/AgentsPool/internal/core"
)

type executionResult struct {
	ExitCode  int
	Err       error
	Cancelled bool
}

type logEmitter func(stream, data string)

func executeTask(ctx context.Context, cfg config.Worker, executionID string, spec core.ExecutorSpec, workspace string, secrets map[string]string, emit logEmitter, logger *slog.Logger) executionResult {
	if !cfg.AllowedExecutors[spec.Kind] {
		return executionResult{ExitCode: -1, Err: fmt.Errorf("executor %q is disabled by worker policy", spec.Kind)}
	}
	workDir, err := safeJoin(workspace, spec.WorkDir)
	if err != nil {
		return executionResult{ExitCode: -1, Err: err}
	}
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return executionResult{ExitCode: -1, Err: fmt.Errorf("create work directory: %w", err)}
	}
	env, err := resolveEnvironment(spec.Env, secrets)
	if err != nil {
		return executionResult{ExitCode: -1, Err: err}
	}

	switch spec.Kind {
	case core.ExecutorShell:
		command := exec.Command(cfg.ShellBinary, "-lc", spec.Script)
		command.Dir = workDir
		command.Env = mergeHostEnvironment(env, cfg.InheritHostEnvironment)
		return runCommand(ctx, command, emit)
	case core.ExecutorProcess:
		if len(spec.Command) == 0 {
			return executionResult{ExitCode: -1, Err: errors.New("process command is empty")}
		}
		command := exec.Command(spec.Command[0], spec.Command[1:]...)
		command.Dir = workDir
		command.Env = mergeHostEnvironment(env, cfg.InheritHostEnvironment)
		return runCommand(ctx, command, emit)
	case core.ExecutorDocker:
		return executeDocker(ctx, cfg, executionID, spec, workspace, env, emit, logger)
	default:
		return executionResult{ExitCode: -1, Err: fmt.Errorf("unsupported executor %q", spec.Kind)}
	}
}

func executeDocker(ctx context.Context, cfg config.Worker, executionID string, spec core.ExecutorSpec, workspace string, env map[string]string, emit logEmitter, logger *slog.Logger) executionResult {
	if !commandExists(cfg.DockerBinary) {
		return executionResult{ExitCode: -1, Err: fmt.Errorf("docker binary %q was not found", cfg.DockerBinary)}
	}
	network := spec.Network
	if network == "" {
		network = "none"
	}
	if network != "none" && !cfg.AllowNetwork {
		return executionResult{ExitCode: -1, Err: fmt.Errorf("docker network access is disabled by worker policy")}
	}
	if network == "host" && !cfg.AllowHostNetwork {
		return executionResult{ExitCode: -1, Err: fmt.Errorf("docker host networking is disabled by worker policy")}
	}
	containerName := "agentspool-" + strings.ReplaceAll(executionID, "_", "-")
	if len(containerName) > 63 {
		containerName = containerName[:63]
	}
	args := []string{"run", "--rm", "--name", containerName, "--network", network, "--mount", "type=bind,src=" + workspace + ",dst=/workspace", "--workdir", "/workspace"}
	if spec.WorkDir != "" {
		args[len(args)-1] = "/workspace/" + filepath.ToSlash(filepath.Clean(spec.WorkDir))
	}
	if spec.CPULimit > 0 {
		args = append(args, "--cpus", strconv.FormatFloat(spec.CPULimit, 'f', -1, 64))
	}
	if spec.MemoryMB > 0 {
		args = append(args, "--memory", fmt.Sprintf("%dm", spec.MemoryMB))
	}
	if spec.User != "" {
		args = append(args, "--user", spec.User)
	}
	if spec.ReadOnlyRoot {
		args = append(args, "--read-only", "--tmpfs", "/tmp:rw,nosuid,nodev,size=1g")
	}
	envFile, err := writeDockerEnvFile(filepath.Dir(workspace), env)
	if err != nil {
		return executionResult{ExitCode: -1, Err: err}
	}
	if envFile != "" {
		defer os.Remove(envFile)
		args = append(args, "--env-file", envFile)
	}
	args = append(args, spec.Image)
	args = append(args, spec.Command...)

	command := exec.Command(cfg.DockerBinary, args...)
	result := runCommand(ctx, command, emit)
	cleanupCtx, cancel := context.WithTimeout(context.Background(), dockerCleanupTimeout)
	defer cancel()
	if err := exec.CommandContext(cleanupCtx, cfg.DockerBinary, "rm", "-f", containerName).Run(); err != nil {
		logger.Debug("docker cleanup completed with non-fatal error", "container", containerName, "error", err)
	}
	return result
}

func writeDockerEnvFile(directory string, env map[string]string) (string, error) {
	if len(env) == 0 {
		return "", nil
	}
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	file, err := os.CreateTemp(directory, ".agentspool-docker-env-*.list")
	if err != nil {
		return "", fmt.Errorf("create Docker env file: %w", err)
	}
	path := file.Name()
	cleanup := func() {
		_ = file.Close()
		_ = os.Remove(path)
	}
	for _, key := range keys {
		value := env[key]
		if strings.ContainsAny(value, "\r\n\x00") {
			cleanup()
			return "", fmt.Errorf("Docker environment variable %q contains a line break or NUL; encode multiline secrets or pass them as an Artifact", key)
		}
		if _, err := fmt.Fprintf(file, "%s=%s\n", key, value); err != nil {
			cleanup()
			return "", fmt.Errorf("write Docker env file: %w", err)
		}
	}
	if err := file.Sync(); err != nil {
		cleanup()
		return "", fmt.Errorf("sync Docker env file: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("close Docker env file: %w", err)
	}
	return path, nil
}

func runCommand(ctx context.Context, command *exec.Cmd, emit logEmitter) executionResult {
	stdout, err := command.StdoutPipe()
	if err != nil {
		return executionResult{ExitCode: -1, Err: err}
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		return executionResult{ExitCode: -1, Err: err}
	}
	configureProcess(command)
	if err := command.Start(); err != nil {
		return executionResult{ExitCode: -1, Err: err}
	}

	var readers sync.WaitGroup
	readers.Add(2)
	go func() {
		defer readers.Done()
		emitReader(stdout, "stdout", emit)
	}()
	go func() {
		defer readers.Done()
		emitReader(stderr, "stderr", emit)
	}()

	waitCh := make(chan error, 1)
	go func() { waitCh <- command.Wait() }()
	var waitErr error
	cancelled := false
	select {
	case waitErr = <-waitCh:
	case <-ctx.Done():
		cancelled = true
		_ = terminateProcess(command)
		waitErr = <-waitCh
	}
	readers.Wait()
	exitCode := 0
	if command.ProcessState != nil {
		exitCode = command.ProcessState.ExitCode()
	} else if waitErr != nil {
		exitCode = -1
	}
	if cancelled {
		return executionResult{ExitCode: exitCode, Err: ctx.Err(), Cancelled: true}
	}
	if waitErr != nil {
		return executionResult{ExitCode: exitCode, Err: waitErr}
	}
	return executionResult{ExitCode: exitCode}
}

func emitReader(reader io.Reader, stream string, emit logEmitter) {
	buffered := bufio.NewReaderSize(reader, 32<<10)
	buffer := make([]byte, 32<<10)
	for {
		count, err := buffered.Read(buffer)
		if count > 0 {
			emit(stream, string(buffer[:count]))
		}
		if err != nil {
			return
		}
	}
}

func mergeHostEnvironment(overrides map[string]string, inheritAll bool) []string {
	values := map[string]string{}
	for _, item := range os.Environ() {
		parts := strings.SplitN(item, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := parts[0]
		if inheritAll || safeInheritedEnvironmentKey(key) {
			values[key] = parts[1]
		}
	}
	for key, value := range overrides {
		values[key] = value
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+values[key])
	}
	return out
}

func safeInheritedEnvironmentKey(key string) bool {
	if strings.HasPrefix(key, "LC_") || strings.HasPrefix(key, "XDG_") {
		return true
	}
	switch key {
	case "PATH", "HOME", "USER", "LOGNAME", "SHELL", "LANG", "LANGUAGE", "TZ", "TERM",
		"TMPDIR", "TMP", "TEMP", "DISPLAY", "WAYLAND_DISPLAY",
		"LD_LIBRARY_PATH", "CUDA_HOME", "CUDA_PATH", "CUDA_VISIBLE_DEVICES",
		"NVIDIA_VISIBLE_DEVICES", "NVIDIA_DRIVER_CAPABILITIES", "ROCM_PATH", "HIP_VISIBLE_DEVICES",
		"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY",
		"http_proxy", "https_proxy", "no_proxy", "all_proxy":
		return true
	default:
		return false
	}
}

func resolveEnvironment(input, secrets map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(input))
	for key, value := range input {
		if strings.ContainsAny(key, "=\x00") || strings.TrimSpace(key) == "" {
			return nil, fmt.Errorf("invalid environment variable name %q", key)
		}
		if strings.HasPrefix(value, "${secret:") && strings.HasSuffix(value, "}") {
			secretName := strings.TrimSuffix(strings.TrimPrefix(value, "${secret:"), "}")
			secret, ok := secrets[secretName]
			if !ok {
				return nil, fmt.Errorf("worker secret %q is not configured", secretName)
			}
			value = secret
		}
		out[key] = value
	}
	return out, nil
}

func loadSecrets(path string) (map[string]string, error) {
	out := map[string]string{}
	if path == "" {
		return out, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" {
			return nil, fmt.Errorf("invalid secrets file line %d", lineNumber)
		}
		out[strings.TrimSpace(parts[0])] = parts[1]
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func safeJoin(root, relative string) (string, error) {
	if relative == "" {
		return root, nil
	}
	if filepath.IsAbs(relative) {
		return "", errors.New("absolute work paths are not allowed")
	}
	cleaned := filepath.Clean(relative)
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", errors.New("work path escapes workspace")
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	candidate, err := filepath.Abs(filepath.Join(root, cleaned))
	if err != nil {
		return "", err
	}
	if candidate != rootAbs && !strings.HasPrefix(candidate, rootAbs+string(filepath.Separator)) {
		return "", errors.New("work path escapes workspace")
	}
	return candidate, nil
}
