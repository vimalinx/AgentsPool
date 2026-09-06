package server

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/vimalinx/AgentsPool/internal/core"
)

func normalizeTaskSpec(spec *core.TaskSpec) error {
	spec.Name = strings.TrimSpace(spec.Name)
	if spec.Name == "" {
		return errors.New("name is required")
	}
	if len(spec.Name) > 200 {
		return errors.New("name must not exceed 200 characters")
	}
	if spec.Queue == "" {
		spec.Queue = "default"
	}
	if len(spec.Queue) > 64 {
		return errors.New("queue must not exceed 64 characters")
	}
	if spec.Priority < -1000 || spec.Priority > 1000 {
		return errors.New("priority must be between -1000 and 1000")
	}
	if spec.MaxAttempts == 0 {
		spec.MaxAttempts = 1
	}
	if spec.MaxAttempts < 1 {
		return errors.New("max_attempts must be positive")
	}
	if spec.TimeoutSeconds < 0 {
		return errors.New("timeout_seconds must be zero or greater")
	}
	if spec.RetryBackoffSeconds == 0 {
		spec.RetryBackoffSeconds = 5
	}
	if spec.RetryBackoffSeconds < 1 {
		return errors.New("retry_backoff_seconds must be positive")
	}
	if spec.SideEffectMode == "" {
		spec.SideEffectMode = core.SideEffectNone
	}
	switch spec.SideEffectMode {
	case core.SideEffectNone, core.SideEffectIdempotent, core.SideEffectNonIdempotent:
	default:
		return fmt.Errorf("unsupported side_effect_mode %q", spec.SideEffectMode)
	}
	if len(spec.IdempotencyKey) > 256 {
		return errors.New("idempotency_key must not exceed 256 characters")
	}
	if len(spec.Labels) > 64 || len(spec.Metadata) > 64 || len(spec.Executor.Env) > 128 {
		return errors.New("too many labels, metadata entries, or environment variables")
	}

	spec.Executor.Kind = strings.ToLower(strings.TrimSpace(spec.Executor.Kind))
	switch spec.Executor.Kind {
	case core.ExecutorShell:
		if strings.TrimSpace(spec.Executor.Script) == "" {
			return errors.New("executor.script is required for shell executor")
		}
		if len(spec.Executor.Script) > 1<<20 {
			return errors.New("executor.script is too large")
		}
		spec.Requirements.Capabilities = appendUnique(spec.Requirements.Capabilities, "executor.shell")
	case core.ExecutorProcess:
		if len(spec.Executor.Command) == 0 || strings.TrimSpace(spec.Executor.Command[0]) == "" {
			return errors.New("executor.command is required for process executor")
		}
		spec.Requirements.Capabilities = appendUnique(spec.Requirements.Capabilities, "executor.process")
	case core.ExecutorDocker:
		if strings.TrimSpace(spec.Executor.Image) == "" {
			return errors.New("executor.image is required for docker executor")
		}
		if spec.Executor.Network == "" {
			spec.Executor.Network = "none"
		}
		switch spec.Executor.Network {
		case "none", "bridge", "host":
		default:
			return errors.New("executor.network must be one of none, bridge, or host")
		}
		spec.Requirements.Capabilities = appendUnique(spec.Requirements.Capabilities, "executor.docker")
	default:
		return fmt.Errorf("unsupported executor kind %q", spec.Executor.Kind)
	}

	if spec.Executor.CPULimit < 0 || spec.Executor.MemoryMB < 0 {
		return errors.New("executor resource limits cannot be negative")
	}
	if spec.Executor.WorkDir != "" {
		if err := validateRelativePath(spec.Executor.WorkDir); err != nil {
			return fmt.Errorf("invalid executor.work_dir: %w", err)
		}
	}
	for i, input := range spec.Inputs {
		if strings.TrimSpace(input.ArtifactID) == "" {
			return fmt.Errorf("inputs[%d].artifact_id is required", i)
		}
		if err := validateRelativePath(input.Path); err != nil {
			return fmt.Errorf("invalid inputs[%d].path: %w", i, err)
		}
	}
	for i, output := range spec.Outputs {
		if strings.TrimSpace(output.Pattern) == "" {
			return fmt.Errorf("outputs[%d].pattern is required", i)
		}
		if err := validateRelativePattern(output.Pattern); err != nil {
			return fmt.Errorf("invalid outputs[%d].pattern: %w", i, err)
		}
	}
	if spec.Requirements.MinCPUThreads < 0 || spec.Requirements.MinMemoryMB < 0 || spec.Requirements.MinDiskMB < 0 {
		return errors.New("resource requirements cannot be negative")
	}
	return nil
}

func validateRelativePath(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("path is required")
	}
	if filepath.IsAbs(value) {
		return errors.New("absolute paths are not allowed")
	}
	cleaned := filepath.Clean(value)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return errors.New("path must stay inside the job workspace")
	}
	if strings.ContainsRune(value, '\x00') {
		return errors.New("path contains a NUL byte")
	}
	return nil
}

func validateRelativePattern(value string) error {
	if err := validateRelativePath(strings.ReplaceAll(value, "**", "x")); err != nil {
		return err
	}
	return nil
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}
