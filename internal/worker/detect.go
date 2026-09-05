package worker

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/qiyuehuajin/agentspool/internal/config"
	"github.com/qiyuehuajin/agentspool/internal/core"
)

type detector struct {
	cfg       config.Worker
	mu        sync.Mutex
	lastTotal uint64
	lastIdle  uint64
}

func newDetector(cfg config.Worker) *detector {
	return &detector{cfg: cfg}
}

func resolveMachineID(dataDir, override string) (string, error) {
	if value := strings.TrimSpace(override); value != "" {
		return value, nil
	}
	for _, candidate := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		if data, err := os.ReadFile(candidate); err == nil {
			if value := strings.TrimSpace(string(data)); value != "" {
				return value, nil
			}
		}
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dataDir, "machine-id")
	if data, err := os.ReadFile(path); err == nil {
		if value := strings.TrimSpace(string(data)); value != "" {
			return value, nil
		}
	}
	hostname, _ := os.Hostname()
	seed := fmt.Sprintf("%s|%s|%d", hostname, dataDir, time.Now().UnixNano())
	sum := sha256.Sum256([]byte(seed))
	value := hex.EncodeToString(sum[:16])
	if err := os.WriteFile(path, []byte(value+"\n"), 0o600); err != nil {
		return "", err
	}
	return value, nil
}

func (d *detector) Static(ctx context.Context) core.NodeCapabilities {
	memoryTotal, _ := readMemory()
	diskTotal, _, _ := diskUsage(d.cfg.DataDir)
	var gpus []core.GPUInfo
	if d.cfg.DetectGPU {
		gpus = detectNVIDIAGPUs(ctx)
	}
	capabilities := []string{}
	if d.cfg.AllowedExecutors[core.ExecutorProcess] {
		capabilities = append(capabilities, "executor.process")
	}
	if d.cfg.AllowedExecutors[core.ExecutorShell] && commandExists(d.cfg.ShellBinary) {
		capabilities = append(capabilities, "executor.shell")
	}
	if d.cfg.AllowedExecutors[core.ExecutorDocker] && dockerAvailable(ctx, d.cfg.DockerBinary) {
		capabilities = append(capabilities, "executor.docker")
	}
	for _, runtimeCommand := range []struct{ command, capability string }{
		{"python3", "runtime.python"}, {"node", "runtime.node"}, {"git", "tool.git"}, {"ffmpeg", "tool.ffmpeg"},
	} {
		if commandExists(runtimeCommand.command) {
			capabilities = append(capabilities, runtimeCommand.capability)
		}
	}
	if os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != "" {
		capabilities = append(capabilities, "gui.session")
	}
	capabilities = append(capabilities, d.cfg.ExtraCapabilities...)
	capabilities = uniqueSorted(capabilities)

	hostname, _ := os.Hostname()
	return core.NodeCapabilities{
		Hostname: hostname, OS: runtime.GOOS, Arch: runtime.GOARCH, CPUThreads: runtime.NumCPU(),
		MemoryTotalMB: memoryTotal, DiskTotalMB: diskTotal, GPUs: gpus,
		Capabilities: capabilities, Labels: cloneStringMap(d.cfg.Labels), Queues: append([]string(nil), d.cfg.Queues...),
		AgentVersion: config.Version,
	}
}

func (d *detector) Dynamic(activeJobs int) core.NodeResources {
	_, memoryAvailable := readMemory()
	_, diskAvailable, _ := diskUsage(d.cfg.DataDir)
	load1 := readLoad1()
	cpuPercent := d.readCPUPercent()
	var gpus []core.GPUInfo
	if d.cfg.DetectGPU {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		gpus = detectNVIDIAGPUs(ctx)
	}
	return core.NodeResources{
		CPUPercent: cpuPercent, Load1: load1, MemoryAvailableMB: memoryAvailable,
		DiskAvailableMB: diskAvailable, GPUs: gpus, ActiveJobs: activeJobs,
		MaxConcurrency: d.cfg.MaxConcurrency, ObservedAt: time.Now().UTC(),
	}
}

func readMemory() (totalMB, availableMB int64) {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	defer file.Close()
	values := map[string]int64{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		value, err := strconv.ParseInt(fields[1], 10, 64)
		if err == nil {
			values[strings.TrimSuffix(fields[0], ":")] = value / 1024
		}
	}
	return values["MemTotal"], values["MemAvailable"]
}

func readLoad1() float64 {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0
	}
	value, _ := strconv.ParseFloat(fields[0], 64)
	return value
}

func (d *detector) readCPUPercent() float64 {
	file, err := os.Open("/proc/stat")
	if err != nil {
		return 0
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	if !scanner.Scan() {
		return 0
	}
	fields := strings.Fields(scanner.Text())
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0
	}
	var values []uint64
	for _, field := range fields[1:] {
		value, err := strconv.ParseUint(field, 10, 64)
		if err != nil {
			return 0
		}
		values = append(values, value)
	}
	var total uint64
	for _, value := range values {
		total += value
	}
	idle := values[3]
	if len(values) > 4 {
		idle += values[4]
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.lastTotal == 0 || total <= d.lastTotal {
		d.lastTotal, d.lastIdle = total, idle
		return 0
	}
	totalDelta := total - d.lastTotal
	idleDelta := idle - d.lastIdle
	d.lastTotal, d.lastIdle = total, idle
	if totalDelta == 0 {
		return 0
	}
	return float64(totalDelta-idleDelta) * 100 / float64(totalDelta)
}

func detectNVIDIAGPUs(ctx context.Context) []core.GPUInfo {
	if !commandExists("nvidia-smi") {
		return nil
	}
	command := exec.CommandContext(ctx, "nvidia-smi", "--query-gpu=index,name,uuid,memory.total,memory.free,driver_version", "--format=csv,noheader,nounits")
	output, err := command.Output()
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	gpus := make([]core.GPUInfo, 0, len(lines))
	for _, line := range lines {
		parts := splitCSVLine(line)
		if len(parts) < 6 {
			continue
		}
		index, _ := strconv.Atoi(strings.TrimSpace(parts[0]))
		total, _ := strconv.ParseInt(strings.TrimSpace(parts[3]), 10, 64)
		free, _ := strconv.ParseInt(strings.TrimSpace(parts[4]), 10, 64)
		gpus = append(gpus, core.GPUInfo{
			Index: index, Name: strings.TrimSpace(parts[1]), UUID: strings.TrimSpace(parts[2]),
			MemoryTotalMB: total, MemoryFreeMB: free, DriverVersion: strings.TrimSpace(parts[5]), ComputeRuntime: "cuda",
		})
	}
	return gpus
}

func splitCSVLine(line string) []string {
	// nvidia-smi's queried fields do not normally contain commas. Preserve a
	// small quoted-field parser for GPU names supplied by unusual drivers.
	parts := []string{}
	var current strings.Builder
	quoted := false
	for _, r := range line {
		switch r {
		case '"':
			quoted = !quoted
		case ',':
			if quoted {
				current.WriteRune(r)
			} else {
				parts = append(parts, current.String())
				current.Reset()
			}
		default:
			current.WriteRune(r)
		}
	}
	parts = append(parts, current.String())
	return parts
}

func dockerAvailable(ctx context.Context, binary string) bool {
	if !commandExists(binary) {
		return false
	}
	checkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return exec.CommandContext(checkCtx, binary, "version", "--format", "{{.Server.Version}}").Run() == nil
}

func commandExists(command string) bool {
	if command == "" {
		return false
	}
	_, err := exec.LookPath(command)
	return err == nil
}

func uniqueSorted(values []string) []string {
	set := map[string]struct{}{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			set[value] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for value := range set {
		out = append(out, value)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func cloneStringMap(source map[string]string) map[string]string {
	out := make(map[string]string, len(source))
	for key, value := range source {
		out[key] = value
	}
	return out
}
