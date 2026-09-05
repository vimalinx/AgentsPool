package server

import (
	"sort"
	"strings"
	"time"

	"github.com/qiyuehuajin/agentspool/internal/core"
)

type ClaimRequest struct {
	Resources         core.NodeResources `json:"resources"`
	CachedArtifactIDs []string           `json:"cached_artifact_ids,omitempty"`
}

type ClaimResponse struct {
	Claimed         bool       `json:"claimed"`
	Task            *core.Task `json:"task,omitempty"`
	LeaseID         string     `json:"lease_id,omitempty"`
	LeaseToken      string     `json:"lease_token,omitempty"`
	ExecutionID     string     `json:"execution_id,omitempty"`
	LeaseExpiresAt  *time.Time `json:"lease_expires_at,omitempty"`
	LeaseTTLSeconds int        `json:"lease_ttl_seconds,omitempty"`
}

func taskMatchesNode(task *core.Task, node *core.Node, cached map[string]struct{}, now time.Time) (bool, int) {
	if task.Status != core.TaskQueued {
		return false, 0
	}
	if task.NextEligibleAt != nil && task.NextEligibleAt.After(now) {
		return false, 0
	}
	if task.Spec.Queue != "" && !core.ContainsString(node.Capabilities.Queues, task.Spec.Queue) {
		return false, 0
	}
	req := task.Spec.Requirements
	if len(req.AllowedNodeIDs) > 0 && !core.ContainsString(req.AllowedNodeIDs, node.ID) {
		return false, 0
	}
	if core.ContainsString(req.ExcludedNodeIDs, node.ID) {
		return false, 0
	}
	if req.OS != "" && !strings.EqualFold(req.OS, node.Capabilities.OS) {
		return false, 0
	}
	if req.Arch != "" && !strings.EqualFold(req.Arch, node.Capabilities.Arch) {
		return false, 0
	}
	if req.MinCPUThreads > node.Capabilities.CPUThreads {
		return false, 0
	}
	if req.MinMemoryMB > node.Resources.MemoryAvailableMB {
		return false, 0
	}
	if req.MinDiskMB > node.Resources.DiskAvailableMB {
		return false, 0
	}
	capSet := core.NormalizedSet(node.Capabilities.Capabilities)
	for _, capability := range req.Capabilities {
		if _, ok := capSet[capability]; !ok {
			return false, 0
		}
	}
	for key, value := range req.Labels {
		if node.Capabilities.Labels[key] != value {
			return false, 0
		}
	}
	score := 0
	if core.ContainsString(req.PreferredNodeIDs, node.ID) {
		score += 10000
	}
	for _, input := range task.Spec.Inputs {
		if _, ok := cached[input.ArtifactID]; ok {
			score += 500
		}
	}
	if req.MinMemoryMB > 0 {
		headroom := node.Resources.MemoryAvailableMB - req.MinMemoryMB
		if headroom > 0 {
			score += minInt(int(headroom/256), 100)
		}
	}
	score -= node.Resources.ActiveJobs * 20
	return true, score
}

func selectTask(state *core.State, node *core.Node, cachedArtifactIDs []string, now time.Time) *core.Task {
	cached := core.NormalizedSet(cachedArtifactIDs)
	type candidate struct {
		task  *core.Task
		score int
	}
	candidates := make([]candidate, 0)
	for _, task := range state.Tasks {
		if matched, score := taskMatchesNode(task, node, cached, now); matched {
			candidates = append(candidates, candidate{task: task, score: score})
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	sort.Slice(candidates, func(i, j int) bool {
		left, right := candidates[i], candidates[j]
		if left.task.Spec.Priority != right.task.Spec.Priority {
			return left.task.Spec.Priority > right.task.Spec.Priority
		}
		if left.score != right.score {
			return left.score > right.score
		}
		if !left.task.CreatedAt.Equal(right.task.CreatedAt) {
			return left.task.CreatedAt.Before(right.task.CreatedAt)
		}
		return left.task.ID < right.task.ID
	})
	return candidates[0].task
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}
