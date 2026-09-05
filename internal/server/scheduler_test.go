package server

import (
	"testing"
	"time"

	"github.com/qiyuehuajin/agentspool/internal/core"
)

func schedulerNode() *core.Node {
	return &core.Node{
		ID:     "node_a",
		Status: core.NodeOnline,
		Capabilities: core.NodeCapabilities{
			OS: "linux", Arch: "amd64", CPUThreads: 16,
			Capabilities: []string{"executor.shell", "python"},
			Labels:       map[string]string{"room": "office"},
			Queues:       []string{"default"},
			GPUs:         []core.GPUInfo{{Index: 0, Name: "Test GPU", MemoryTotalMB: 12288}},
		},
		Resources: core.NodeResources{
			MemoryAvailableMB: 32000, DiskAvailableMB: 100000,
			GPUs:           []core.GPUInfo{{Index: 0, Name: "Test GPU", MemoryTotalMB: 12288, MemoryFreeMB: 10000}},
			MaxConcurrency: 4,
		},
	}
}

func queuedTask(id string, created time.Time) *core.Task {
	return &core.Task{ID: id, Status: core.TaskQueued, CreatedAt: created, Spec: core.TaskSpec{Queue: "default"}}
}

func TestTaskMatchesNodeRequirements(t *testing.T) {
	now := time.Now().UTC()
	node := schedulerNode()
	task := queuedTask("task_ok", now)
	task.Spec.Requirements = core.Requirements{
		Capabilities: []string{"executor.shell", "python"}, OS: "LINUX", Arch: "amd64",
		MinCPUThreads: 8, MinMemoryMB: 16000, MinDiskMB: 1000,
		Labels: map[string]string{"room": "office"},
	}
	matched, _ := taskMatchesNode(task, node, nil, now)
	if !matched {
		t.Fatal("expected task to match capable node")
	}
	task.Spec.Requirements.MinMemoryMB = 64000
	matched, _ = taskMatchesNode(task, node, nil, now)
	if matched {
		t.Fatal("task should not match insufficient available memory")
	}
}

func TestGPUTelemetryDoesNotAffectEligibilityOrScore(t *testing.T) {
	now := time.Now().UTC()
	task := queuedTask("task_gpu_neutral", now)
	withGPU := schedulerNode()
	withoutGPU := schedulerNode()
	withoutGPU.Capabilities.GPUs = nil
	withoutGPU.Resources.GPUs = nil

	matchedWith, scoreWith := taskMatchesNode(task, withGPU, nil, now)
	matchedWithout, scoreWithout := taskMatchesNode(task, withoutGPU, nil, now)
	if !matchedWith || !matchedWithout {
		t.Fatal("GPU telemetry must not change task eligibility")
	}
	if scoreWith != scoreWithout {
		t.Fatalf("GPU telemetry changed scheduling score: with=%d without=%d", scoreWith, scoreWithout)
	}
}

func TestSelectTaskUsesPriorityBeforeLocality(t *testing.T) {
	now := time.Now().UTC()
	state := core.NewState()
	local := queuedTask("task_local", now.Add(-time.Minute))
	local.Spec.Inputs = []core.ArtifactInput{{ArtifactID: "art_cached", Path: "input.bin"}}
	local.Spec.Priority = 1
	high := queuedTask("task_high", now)
	high.Spec.Priority = 2
	state.Tasks[local.ID] = local
	state.Tasks[high.ID] = high
	selected := selectTask(&state, schedulerNode(), []string{"art_cached"}, now)
	if selected == nil || selected.ID != high.ID {
		t.Fatalf("expected higher-priority task, got %#v", selected)
	}
}

func TestSelectTaskUsesLocalityAndFIFO(t *testing.T) {
	now := time.Now().UTC()
	state := core.NewState()
	old := queuedTask("task_old", now.Add(-2*time.Minute))
	cached := queuedTask("task_cached", now.Add(-time.Minute))
	cached.Spec.Inputs = []core.ArtifactInput{{ArtifactID: "art_cached", Path: "input.bin"}}
	state.Tasks[old.ID] = old
	state.Tasks[cached.ID] = cached
	selected := selectTask(&state, schedulerNode(), []string{"art_cached"}, now)
	if selected == nil || selected.ID != cached.ID {
		t.Fatalf("expected cached-input task, got %#v", selected)
	}
	cached.Spec.Inputs = nil
	selected = selectTask(&state, schedulerNode(), nil, now)
	if selected == nil || selected.ID != old.ID {
		t.Fatalf("expected oldest equal task, got %#v", selected)
	}
}
