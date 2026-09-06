package worker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vimalinx/AgentsPool/internal/config"
)

func TestGPUDetectionIsTelemetryOnly(t *testing.T) {
	binDir := t.TempDir()
	fakeNvidiaSMI := filepath.Join(binDir, "nvidia-smi")
	script := "#!/bin/sh\nprintf '%s\\n' '0, Test GPU, GPU-test, 16384, 12288, 999.1'\n"
	if err := os.WriteFile(fakeNvidiaSMI, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)

	cfg := config.DefaultWorker()
	cfg.DataDir = t.TempDir()
	cfg.AllowedExecutors = map[string]bool{}
	cfg.DetectGPU = true
	d := newDetector(cfg)
	capabilities := d.Static(context.Background())
	if len(capabilities.GPUs) != 1 || capabilities.GPUs[0].Name != "Test GPU" {
		t.Fatalf("expected GPU telemetry, got %#v", capabilities.GPUs)
	}
	for _, capability := range capabilities.Capabilities {
		if strings.HasPrefix(capability, "gpu.") {
			t.Fatalf("GPU detection leaked into scheduling capability %q", capability)
		}
	}

	resources := d.Dynamic(0)
	if len(resources.GPUs) != 1 || resources.GPUs[0].MemoryFreeMB != 12288 {
		t.Fatalf("expected dynamic GPU telemetry, got %#v", resources.GPUs)
	}
}
