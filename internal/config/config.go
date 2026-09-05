package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const Version = "0.4.0-alpha.4"

type Server struct {
	ListenAddress         string
	DataDir               string
	AdminToken            string
	AllocatorToken        string
	JoinToken             string
	LeaseTTL              time.Duration
	NodeTimeout           time.Duration
	ReaperInterval        time.Duration
	MaxRequestBytes       int64
	MaxArtifactBytes      int64
	EventRetention        int
	TLSCertFile           string
	TLSKeyFile            string
	PublicMetrics         bool
	SandboxLogRetainBytes int64
}

type Worker struct {
	ServerURL               string
	DataDir                 string
	Name                    string
	MachineID               string
	JoinToken               string
	Labels                  map[string]string
	Queues                  []string
	ExtraCapabilities       []string
	DetectGPU               bool
	MaxConcurrency          int
	PollInterval            time.Duration
	HeartbeatInterval       time.Duration
	RequestTimeout          time.Duration
	TransferTimeout         time.Duration
	AllowedExecutors        map[string]bool
	InheritHostEnvironment  bool
	AllowNetwork            bool
	AllowHostNetwork        bool
	DockerBinary            string
	ShellBinary             string
	KeepJobs                bool
	KeepFailedJobs          bool
	MaxOutputBytes          int64
	MaxLogChunkBytes        int
	ArtifactCacheMaxBytes   int64
	SecretsFile             string
	InsecureSkipTLSVerify   bool
	GracefulShutdownTimeout time.Duration
	SandboxSocket           string
}

func DefaultServer() Server {
	return Server{
		ListenAddress:         envOr("AGENTSPOOL_LISTEN", "127.0.0.1:8090"),
		DataDir:               envOr("AGENTSPOOL_DATA_DIR", defaultServerDataDir()),
		AdminToken:            os.Getenv("AGENTSPOOL_ADMIN_TOKEN"),
		AllocatorToken:        os.Getenv("AGENTSPOOL_ALLOCATOR_TOKEN"),
		JoinToken:             os.Getenv("AGENTSPOOL_JOIN_TOKEN"),
		LeaseTTL:              durationEnv("AGENTSPOOL_LEASE_TTL", 45*time.Second),
		NodeTimeout:           durationEnv("AGENTSPOOL_NODE_TIMEOUT", 35*time.Second),
		ReaperInterval:        durationEnv("AGENTSPOOL_REAPER_INTERVAL", 2*time.Second),
		MaxRequestBytes:       int64Env("AGENTSPOOL_MAX_REQUEST_BYTES", 0),
		MaxArtifactBytes:      int64Env("AGENTSPOOL_MAX_ARTIFACT_BYTES", 0),
		EventRetention:        intEnv("AGENTSPOOL_EVENT_RETENTION", 0),
		TLSCertFile:           os.Getenv("AGENTSPOOL_TLS_CERT"),
		TLSKeyFile:            os.Getenv("AGENTSPOOL_TLS_KEY"),
		PublicMetrics:         boolEnv("AGENTSPOOL_PUBLIC_METRICS", false),
		SandboxLogRetainBytes: int64Env("AGENTSPOOL_SANDBOX_LOG_RETAIN_BYTES", 0),
	}
}

func DefaultWorker() Worker {
	return Worker{
		ServerURL:               strings.TrimRight(envOr("AGENTSPOOL_SERVER", "http://127.0.0.1:8090"), "/"),
		DataDir:                 envOr("AGENTSPOOL_WORKER_DATA_DIR", defaultWorkerDataDir()),
		Name:                    envOr("AGENTSPOOL_WORKER_NAME", hostname()),
		MachineID:               os.Getenv("AGENTSPOOL_MACHINE_ID"),
		JoinToken:               os.Getenv("AGENTSPOOL_JOIN_TOKEN"),
		Labels:                  parseKeyValues(os.Getenv("AGENTSPOOL_WORKER_LABELS")),
		Queues:                  parseCSVDefault(os.Getenv("AGENTSPOOL_WORKER_QUEUES"), []string{"default"}),
		ExtraCapabilities:       parseCSV(os.Getenv("AGENTSPOOL_WORKER_CAPABILITIES")),
		DetectGPU:               boolEnv("AGENTSPOOL_WORKER_DETECT_GPU", false),
		MaxConcurrency:          intEnv("AGENTSPOOL_WORKER_MAX_CONCURRENCY", max(1, runtime.NumCPU()/2)),
		PollInterval:            durationEnv("AGENTSPOOL_WORKER_POLL_INTERVAL", 1500*time.Millisecond),
		HeartbeatInterval:       durationEnv("AGENTSPOOL_WORKER_HEARTBEAT_INTERVAL", 10*time.Second),
		RequestTimeout:          durationEnv("AGENTSPOOL_WORKER_REQUEST_TIMEOUT", 30*time.Second),
		TransferTimeout:         durationEnv("AGENTSPOOL_WORKER_TRANSFER_TIMEOUT", 0),
		AllowedExecutors:        parseBoolSet(envOr("AGENTSPOOL_WORKER_ALLOWED_EXECUTORS", "shell")),
		InheritHostEnvironment:  boolEnv("AGENTSPOOL_WORKER_INHERIT_HOST_ENV", false),
		AllowNetwork:            boolEnv("AGENTSPOOL_WORKER_ALLOW_NETWORK", false),
		AllowHostNetwork:        boolEnv("AGENTSPOOL_WORKER_ALLOW_HOST_NETWORK", false),
		DockerBinary:            envOr("AGENTSPOOL_DOCKER_BINARY", "docker"),
		ShellBinary:             envOr("AGENTSPOOL_SHELL_BINARY", "/bin/sh"),
		KeepJobs:                boolEnv("AGENTSPOOL_WORKER_KEEP_JOBS", false),
		KeepFailedJobs:          boolEnv("AGENTSPOOL_WORKER_KEEP_FAILED_JOBS", true),
		MaxOutputBytes:          int64Env("AGENTSPOOL_WORKER_MAX_OUTPUT_BYTES", 0),
		MaxLogChunkBytes:        intEnv("AGENTSPOOL_WORKER_MAX_LOG_CHUNK_BYTES", 64<<10),
		ArtifactCacheMaxBytes:   int64Env("AGENTSPOOL_WORKER_CACHE_MAX_BYTES", 0),
		SecretsFile:             os.Getenv("AGENTSPOOL_WORKER_SECRETS_FILE"),
		InsecureSkipTLSVerify:   boolEnv("AGENTSPOOL_INSECURE_SKIP_TLS_VERIFY", false),
		GracefulShutdownTimeout: durationEnv("AGENTSPOOL_WORKER_GRACEFUL_TIMEOUT", 0),
		SandboxSocket:           envOr("AGENTSPOOL_SANDBOX_SOCKET", "/run/agentspool/sandboxd.sock"),
	}
}

func (c Server) Validate() error {
	if c.SandboxLogRetainBytes < 0 {
		return errors.New("sandbox log retention cannot be negative")
	}
	if c.ListenAddress == "" {
		return errors.New("listen address is required")
	}
	if c.DataDir == "" {
		return errors.New("data directory is required")
	}
	if len(c.AdminToken) < 24 {
		return errors.New("admin token must contain at least 24 characters")
	}
	if c.AllocatorToken != "" && len(c.AllocatorToken) < 24 {
		return errors.New("allocator token must contain at least 24 characters")
	}
	if len(c.JoinToken) < 24 {
		return errors.New("join token must contain at least 24 characters")
	}
	for name, token := range map[string]string{"admin": c.AdminToken, "allocator": c.AllocatorToken, "join": c.JoinToken} {
		value := strings.ToLower(strings.TrimSpace(token))
		if strings.HasPrefix(value, "replace-with-") || strings.Contains(value, "changeme") || strings.Contains(value, "change-me") {
			return fmt.Errorf("%s token is an example placeholder; generate a new token with agentspool token", name)
		}
	}
	if c.AdminToken == c.JoinToken || (c.AllocatorToken != "" && (c.AllocatorToken == c.AdminToken || c.AllocatorToken == c.JoinToken)) {
		return errors.New("admin, allocator and join tokens must be different")
	}
	if (c.TLSCertFile == "") != (c.TLSKeyFile == "") {
		return errors.New("TLS certificate and key must be configured together")
	}
	if c.LeaseTTL < 10*time.Second {
		return errors.New("lease TTL must be at least 10 seconds")
	}
	if c.NodeTimeout < c.LeaseTTL/2 {
		return errors.New("node timeout must be at least half the lease TTL")
	}
	if c.ReaperInterval < 100*time.Millisecond {
		return errors.New("reaper interval must be at least 100ms")
	}
	if c.MaxRequestBytes < 0 {
		return errors.New("maximum request size cannot be negative")
	}
	if c.MaxArtifactBytes < 0 {
		return errors.New("maximum artifact size cannot be negative")
	}
	if c.EventRetention < 0 {
		return errors.New("event retention cannot be negative")
	}
	return nil
}

func (c Worker) Validate() error {
	if c.ServerURL == "" {
		return errors.New("server URL is required")
	}
	if c.DataDir == "" {
		return errors.New("worker data directory is required")
	}
	if c.SandboxSocket != "" && !filepath.IsAbs(c.SandboxSocket) {
		return errors.New("sandbox socket path must be absolute")
	}
	if c.Name == "" {
		return errors.New("worker name is required")
	}
	if c.MaxConcurrency < 1 {
		return errors.New("max concurrency must be positive")
	}
	if c.PollInterval < 100*time.Millisecond {
		return errors.New("poll interval must be at least 100ms")
	}
	if c.HeartbeatInterval < time.Second {
		return errors.New("heartbeat interval must be at least 1s")
	}
	if c.RequestTimeout < time.Second {
		return errors.New("request timeout must be at least 1s")
	}
	if c.TransferTimeout < 0 {
		return errors.New("transfer timeout cannot be negative")
	}
	if c.GracefulShutdownTimeout < 0 {
		return errors.New("graceful shutdown timeout cannot be negative")
	}
	if c.MaxOutputBytes < 0 || c.ArtifactCacheMaxBytes < 0 {
		return errors.New("output and cache size limits cannot be negative")
	}
	if c.MaxLogChunkBytes < 1024 || c.MaxLogChunkBytes > 256<<10 {
		return errors.New("maximum log chunk size must be between 1 KiB and 256 KiB")
	}
	if len(c.AllowedExecutors) == 0 {
		return errors.New("at least one executor must be allowed")
	}
	for executor, enabled := range c.AllowedExecutors {
		if !enabled {
			continue
		}
		switch executor {
		case "shell", "process", "docker":
		default:
			return fmt.Errorf("unsupported allowed executor %q", executor)
		}
	}
	if c.AllowedExecutors["shell"] && c.ShellBinary == "" {
		return errors.New("shell binary is required when shell executor is enabled")
	}
	if c.AllowedExecutors["docker"] && c.DockerBinary == "" {
		return errors.New("Docker binary is required when Docker executor is enabled")
	}
	return nil
}

func defaultServerDataDir() string {
	if runtime.GOOS == "windows" {
		return filepath.Join(os.Getenv("ProgramData"), "AgentsPool", "server")
	}
	return "/var/lib/agentspool/server"
}

func defaultWorkerDataDir() string {
	if runtime.GOOS == "windows" {
		return filepath.Join(os.Getenv("ProgramData"), "AgentsPool", "worker")
	}
	return "/var/lib/agentspool/worker"
}

func hostname() string {
	name, err := os.Hostname()
	if err != nil || name == "" {
		return "agentspool-worker"
	}
	return name
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func durationEnv(key string, fallback time.Duration) time.Duration {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func intEnv(key string, fallback int) int {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func int64Env(key string, fallback int64) int64 {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return fallback
	}
	return parsed
}

func boolEnv(key string, fallback bool) bool {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func parseCSV(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	seen := map[string]struct{}{}
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if _, ok := seen[part]; ok {
			continue
		}
		seen[part] = struct{}{}
		out = append(out, part)
	}
	return out
}

func parseCSVDefault(value string, fallback []string) []string {
	out := parseCSV(value)
	if len(out) == 0 {
		return fallback
	}
	return out
}

func parseKeyValues(value string) map[string]string {
	out := map[string]string{}
	for _, item := range parseCSV(value) {
		parts := strings.SplitN(item, "=", 2)
		if len(parts) == 2 && strings.TrimSpace(parts[0]) != "" {
			out[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
	}
	return out
}

func parseBoolSet(value string) map[string]bool {
	out := map[string]bool{}
	for _, item := range parseCSV(value) {
		out[item] = true
	}
	return out
}

func ParseLabels(value string) (map[string]string, error) {
	out := map[string]string{}
	if strings.TrimSpace(value) == "" {
		return out, nil
	}
	for _, item := range strings.Split(value, ",") {
		parts := strings.SplitN(item, "=", 2)
		if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" {
			return nil, fmt.Errorf("invalid label %q; expected key=value", item)
		}
		out[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
	}
	return out, nil
}

func ParseCSV(value string) []string                { return parseCSV(value) }
func ParseExecutorSet(value string) map[string]bool { return parseBoolSet(value) }
