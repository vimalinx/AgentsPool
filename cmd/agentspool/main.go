package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/vimalinx/AgentsPool/internal/client"
	"github.com/vimalinx/AgentsPool/internal/config"
	"github.com/vimalinx/AgentsPool/internal/core"
	"github.com/vimalinx/AgentsPool/internal/sandboxd"
	"github.com/vimalinx/AgentsPool/internal/server"
	"github.com/vimalinx/AgentsPool/internal/store"
	"github.com/vimalinx/AgentsPool/internal/worker"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		printUsage()
		return nil
	}
	switch args[0] {
	case "server":
		return runServer(args[1:])
	case "worker":
		return runWorker(args[1:])
	case "sandboxd":
		return runSandboxd(args[1:])
	case "token":
		return runToken(args[1:])
	case "configure":
		return runConfigure(args[1:])
	case "status":
		return runStatus(args[1:])
	case "nodes", "node":
		return runNodes(args[1:])
	case "tasks", "task":
		return runTasks(args[1:])
	case "artifacts", "artifact":
		return runArtifacts(args[1:])
	case "events":
		return runEvents(args[1:])
	case "tool":
		return runTool(args[1:])
	case "version", "--version", "-v":
		fmt.Println(config.Version)
		return nil
	case "help", "--help", "-h":
		printUsage()
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func printUsage() {
	fmt.Printf(`AgentsPool %s — runtime-neutral Agent compute coordinator

Usage:

Core Agent interface:
  agentspool tool list [--category CATEGORY] --json
  agentspool tool describe TOOL_NAME --json
  agentspool tool TOOL_NAME [flags]          Call a generic compute tool

Services:
  agentspool server [flags]                 Run the control plane
  agentspool worker [flags]                 Run an execution worker
  agentspool sandboxd [flags]               Run the root sandbox helper

Administration:
  agentspool token                          Generate a secure token
  agentspool configure [flags]              Save a runtime-neutral client profile
  agentspool status [flags]                 Show pool summary
  agentspool nodes <list|get|drain|enable|disable|delete>
  agentspool artifacts <upload|list|get|download|delete|gc>
  agentspool events [flags]

Compatibility Task API:
  agentspool tasks <submit|list|get|logs|wait|cancel|retry|delete>

Other:
  agentspool version

Use "agentspool <command> --help" for command-specific flags.
`, config.Version)
}

type boolFlag interface {
	IsBoolFlag() bool
}

// parseInterspersed keeps the standard-library flag package while accepting
// conventional CLI forms such as "tasks logs TASK_ID --follow". The flag
// package normally stops at the first positional argument.
func parseInterspersed(fs *flag.FlagSet, args []string) error {
	options := make([]string, 0, len(args))
	positionals := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if argument == "--" {
			positionals = append(positionals, args[index+1:]...)
			break
		}
		if argument == "-" || !strings.HasPrefix(argument, "-") {
			positionals = append(positionals, argument)
			continue
		}

		options = append(options, argument)
		nameValue := strings.TrimLeft(argument, "-")
		name, _, hasEquals := strings.Cut(nameValue, "=")
		registered := fs.Lookup(name)
		if registered == nil || hasEquals {
			continue
		}
		if value, ok := registered.Value.(boolFlag); ok && value.IsBoolFlag() {
			continue
		}
		if index+1 < len(args) {
			index++
			options = append(options, args[index])
		}
	}
	return fs.Parse(append(options, positionals...))
}

func runToken(args []string) error {
	fs := flag.NewFlagSet("token", flag.ContinueOnError)
	bytesCount := fs.Int("bytes", 32, "number of random bytes")
	if err := parseInterspersed(fs, args); err != nil {
		return err
	}
	if *bytesCount < 24 || *bytesCount > 128 {
		return errors.New("--bytes must be between 24 and 128")
	}
	token, err := core.NewSecret(*bytesCount)
	if err != nil {
		return err
	}
	fmt.Println(token)
	return nil
}

func runServer(args []string) error {
	cfg := config.DefaultServer()
	fs := flag.NewFlagSet("server", flag.ContinueOnError)
	fs.StringVar(&cfg.ListenAddress, "listen", cfg.ListenAddress, "HTTP listen address")
	fs.StringVar(&cfg.DataDir, "data-dir", cfg.DataDir, "persistent server data directory")
	fs.StringVar(&cfg.AdminToken, "admin-token", cfg.AdminToken, "admin bearer token (or AGENTSPOOL_ADMIN_TOKEN)")
	fs.StringVar(&cfg.AllocatorToken, "allocator-token", cfg.AllocatorToken, "allocator bearer token (or AGENTSPOOL_ALLOCATOR_TOKEN)")
	fs.StringVar(&cfg.JoinToken, "join-token", cfg.JoinToken, "worker join token (or AGENTSPOOL_JOIN_TOKEN)")
	adminTokenFile := fs.String("admin-token-file", os.Getenv("AGENTSPOOL_ADMIN_TOKEN_FILE"), "file containing the admin bearer token")
	allocatorTokenFile := fs.String("allocator-token-file", os.Getenv("AGENTSPOOL_ALLOCATOR_TOKEN_FILE"), "file containing the allocator bearer token")
	joinTokenFile := fs.String("join-token-file", os.Getenv("AGENTSPOOL_JOIN_TOKEN_FILE"), "file containing the worker join token")
	fs.DurationVar(&cfg.LeaseTTL, "lease-ttl", cfg.LeaseTTL, "worker lease time-to-live")
	fs.DurationVar(&cfg.NodeTimeout, "node-timeout", cfg.NodeTimeout, "heartbeat timeout before node becomes offline")
	fs.Int64Var(&cfg.MaxArtifactBytes, "max-artifact-bytes", cfg.MaxArtifactBytes, "optional artifact upload size limit; 0 uses available hardware")
	fs.StringVar(&cfg.TLSCertFile, "tls-cert", cfg.TLSCertFile, "TLS certificate file")
	fs.StringVar(&cfg.TLSKeyFile, "tls-key", cfg.TLSKeyFile, "TLS private key file")
	fs.BoolVar(&cfg.PublicMetrics, "public-metrics", cfg.PublicMetrics, "allow unauthenticated /metrics")
	logFormat := fs.String("log-format", envOr("AGENTSPOOL_LOG_FORMAT", "text"), "text or json")
	logLevel := fs.String("log-level", envOr("AGENTSPOOL_LOG_LEVEL", "info"), "debug, info, warn, or error")
	if err := parseInterspersed(fs, args); err != nil {
		return err
	}
	var err error
	if cfg.AdminToken, err = resolveSecret(cfg.AdminToken, *adminTokenFile, "admin"); err != nil {
		return err
	}
	if cfg.AllocatorToken, err = resolveSecret(cfg.AllocatorToken, *allocatorTokenFile, "allocator"); err != nil {
		return err
	}
	if cfg.JoinToken, err = resolveSecret(cfg.JoinToken, *joinTokenFile, "join"); err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	logger := newLogger(*logFormat, *logLevel)
	stateStore, err := store.Open(cfg.DataDir)
	if err != nil {
		return err
	}
	controlPlane, err := server.New(cfg, stateStore, logger)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return controlPlane.Run(ctx)
}

func runWorker(args []string) error {
	cfg := config.DefaultWorker()
	fs := flag.NewFlagSet("worker", flag.ContinueOnError)
	fs.StringVar(&cfg.ServerURL, "server", cfg.ServerURL, "control-plane URL")
	fs.StringVar(&cfg.DataDir, "data-dir", cfg.DataDir, "worker state/cache/job directory")
	fs.StringVar(&cfg.Name, "name", cfg.Name, "human-readable worker name")
	fs.StringVar(&cfg.MachineID, "machine-id", cfg.MachineID, "stable machine identifier override")
	fs.StringVar(&cfg.JoinToken, "join-token", cfg.JoinToken, "worker join token used only for registration")
	joinTokenFile := fs.String("join-token-file", os.Getenv("AGENTSPOOL_JOIN_TOKEN_FILE"), "file containing the worker join token")
	labels := fs.String("labels", formatMap(cfg.Labels), "comma-separated key=value node labels")
	queues := fs.String("queues", strings.Join(cfg.Queues, ","), "comma-separated queues")
	capabilities := fs.String("capabilities", strings.Join(cfg.ExtraCapabilities, ","), "additional advertised capabilities")
	executors := fs.String("executors", boolSetString(cfg.AllowedExecutors), "allowed executors: shell,process,docker")
	fs.BoolVar(&cfg.DetectGPU, "detect-gpu", cfg.DetectGPU, "record GPU telemetry only; never affects scheduling or execution")
	fs.IntVar(&cfg.MaxConcurrency, "max-concurrency", cfg.MaxConcurrency, "maximum simultaneous jobs")
	fs.BoolVar(&cfg.InheritHostEnvironment, "inherit-host-env", cfg.InheritHostEnvironment, "expose the worker process environment to native tasks (trusted tasks only)")
	fs.DurationVar(&cfg.PollInterval, "poll-interval", cfg.PollInterval, "idle claim polling interval")
	fs.DurationVar(&cfg.HeartbeatInterval, "heartbeat-interval", cfg.HeartbeatInterval, "worker heartbeat interval")
	fs.DurationVar(&cfg.RequestTimeout, "request-timeout", cfg.RequestTimeout, "control API request timeout")
	fs.DurationVar(&cfg.TransferTimeout, "transfer-timeout", cfg.TransferTimeout, "optional artifact upload/download timeout; 0 disables the deadline")
	fs.BoolVar(&cfg.AllowNetwork, "allow-network", cfg.AllowNetwork, "allow Docker bridge network tasks")
	fs.BoolVar(&cfg.AllowHostNetwork, "allow-host-network", cfg.AllowHostNetwork, "allow Docker host-network tasks")
	fs.BoolVar(&cfg.KeepJobs, "keep-jobs", cfg.KeepJobs, "retain all job workspaces")
	fs.BoolVar(&cfg.KeepFailedJobs, "keep-failed-jobs", cfg.KeepFailedJobs, "retain failed job workspaces")
	fs.Int64Var(&cfg.MaxOutputBytes, "max-output-bytes", cfg.MaxOutputBytes, "optional matched output size limit; 0 uses available hardware")
	fs.Int64Var(&cfg.ArtifactCacheMaxBytes, "cache-max-bytes", cfg.ArtifactCacheMaxBytes, "optional local Artifact cache limit; 0 uses available hardware")
	fs.StringVar(&cfg.SecretsFile, "secrets-file", cfg.SecretsFile, "local KEY=VALUE secrets file")
	fs.BoolVar(&cfg.InsecureSkipTLSVerify, "insecure-skip-tls-verify", cfg.InsecureSkipTLSVerify, "disable TLS certificate verification (lab use only)")
	fs.DurationVar(&cfg.GracefulShutdownTimeout, "graceful-timeout", cfg.GracefulShutdownTimeout, "optional active-job drain deadline; 0 waits for completion")
	fs.StringVar(&cfg.SandboxSocket, "sandbox-socket", cfg.SandboxSocket, "sandbox helper Unix socket (empty disables sandbox operations)")
	logFormat := fs.String("log-format", envOr("AGENTSPOOL_LOG_FORMAT", "text"), "text or json")
	logLevel := fs.String("log-level", envOr("AGENTSPOOL_LOG_LEVEL", "info"), "debug, info, warn, or error")
	if err := parseInterspersed(fs, args); err != nil {
		return err
	}
	var err error
	if cfg.JoinToken, err = resolveSecret(cfg.JoinToken, *joinTokenFile, "join"); err != nil {
		return err
	}
	parsedLabels, err := config.ParseLabels(*labels)
	if err != nil {
		return err
	}
	cfg.Labels = parsedLabels
	cfg.Queues = config.ParseCSV(*queues)
	if len(cfg.Queues) == 0 {
		cfg.Queues = []string{"default"}
	}
	cfg.ExtraCapabilities = config.ParseCSV(*capabilities)
	cfg.AllowedExecutors = config.ParseExecutorSet(*executors)
	cfg.ServerURL = strings.TrimRight(cfg.ServerURL, "/")
	if err := cfg.Validate(); err != nil {
		return err
	}
	logger := newLogger(*logFormat, *logLevel)
	executionWorker, err := worker.New(cfg, logger)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return executionWorker.Run(ctx)
}

func runSandboxd(args []string) error {
	fs := flag.NewFlagSet("sandboxd", flag.ContinueOnError)
	socket := fs.String("socket", envOr("AGENTSPOOL_SANDBOX_SOCKET", "/run/agentspool/sandboxd.sock"), "Unix socket path")
	dataDir := fs.String("data-dir", envOr("AGENTSPOOL_SANDBOX_DATA_DIR", "/var/lib/agentspool/sandboxes"), "persistent sandbox data directory")
	workerUser := fs.String("worker-user", envOr("AGENTSPOOL_SANDBOX_WORKER_USER", "agentspool"), "unprivileged sandbox user")
	transferRoot := fs.String("transfer-root", envOr("AGENTSPOOL_SANDBOX_TRANSFER_ROOT", "/var/lib/agentspool/worker/transfers"), "fixed Worker transfer directory")
	maxOutput := fs.Int64("max-output-bytes", 0, "inline output preview bytes per stream; 0 uses 64 KiB; full output is stored as logs")
	logRetain := fs.Int64("log-retain-bytes", 0, "optional retained log bytes per stream; 0 retains all disk segments")
	logFormat := fs.String("log-format", envOr("AGENTSPOOL_LOG_FORMAT", "text"), "text or json")
	logLevel := fs.String("log-level", envOr("AGENTSPOOL_LOG_LEVEL", "info"), "debug, info, warn, or error")
	if err := parseInterspersed(fs, args); err != nil {
		return err
	}
	helper, err := sandboxd.New(sandboxd.Config{SocketPath: *socket, DataDir: *dataDir, WorkerUser: *workerUser, MaxOutputBytes: *maxOutput, TransferRoot: *transferRoot, LogRetainBytes: *logRetain}, newLogger(*logFormat, *logLevel))
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return helper.Run(ctx)
}

type adminOptions struct {
	server    string
	token     string
	tokenFile string
	timeout   time.Duration
	insecure  bool
}

func addAdminFlags(fs *flag.FlagSet) *adminOptions {
	options := &adminOptions{}
	fs.StringVar(&options.server, "server", envOr("AGENTSPOOL_SERVER", "http://127.0.0.1:8090"), "control-plane URL")
	fs.StringVar(&options.token, "token", os.Getenv("AGENTSPOOL_ADMIN_TOKEN"), "admin bearer token")
	fs.StringVar(&options.tokenFile, "token-file", os.Getenv("AGENTSPOOL_ADMIN_TOKEN_FILE"), "file containing the admin bearer token")
	fs.DurationVar(&options.timeout, "timeout", 30*time.Second, "request timeout")
	fs.BoolVar(&options.insecure, "insecure-skip-tls-verify", false, "disable TLS certificate verification")
	return options
}

func (o *adminOptions) client() (*client.Client, error) {
	token, err := resolveSecret(o.token, o.tokenFile, "admin")
	if err != nil {
		return nil, err
	}
	if token == "" {
		return nil, errors.New("admin token is required via --token, --token-file, AGENTSPOOL_ADMIN_TOKEN, or AGENTSPOOL_ADMIN_TOKEN_FILE")
	}
	return client.New(o.server, token, o.timeout, o.insecure)
}

func runStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	options := addAdminFlags(fs)
	if err := parseInterspersed(fs, args); err != nil {
		return err
	}
	api, err := options.client()
	if err != nil {
		return err
	}
	var response any
	if err := api.DoJSON(context.Background(), http.MethodGet, "/api/v1/status", nil, nil, &response, nil); err != nil {
		return err
	}
	return printJSON(response)
}

func runNodes(args []string) error {
	if len(args) == 0 {
		return errors.New("nodes requires list, get, drain, enable, disable, or delete")
	}
	action := args[0]
	fs := flag.NewFlagSet("nodes "+action, flag.ContinueOnError)
	options := addAdminFlags(fs)
	status := fs.String("status", "", "filter node status (list)")
	if err := parseInterspersed(fs, args[1:]); err != nil {
		return err
	}
	api, err := options.client()
	if err != nil {
		return err
	}
	ctx := context.Background()
	switch action {
	case "list":
		query := url.Values{}
		if *status != "" {
			query.Set("status", *status)
		}
		var response any
		if err := api.DoJSON(ctx, http.MethodGet, "/api/v1/nodes", query, nil, &response, nil); err != nil {
			return err
		}
		return printJSON(response)
	case "get", "drain", "enable", "disable", "delete":
		if fs.NArg() != 1 {
			return fmt.Errorf("nodes %s requires NODE_ID", action)
		}
		nodeID := fs.Arg(0)
		method, path := http.MethodPost, "/api/v1/nodes/"+nodeID+"/"+action
		if action == "get" {
			method, path = http.MethodGet, "/api/v1/nodes/"+nodeID
		}
		if action == "delete" {
			method, path = http.MethodDelete, "/api/v1/nodes/"+nodeID
		}
		var response any
		var out any = &response
		if action == "delete" {
			out = nil
		}
		if err := api.DoJSON(ctx, method, path, nil, nil, out, nil); err != nil {
			return err
		}
		if action == "delete" {
			fmt.Println("deleted", nodeID)
			return nil
		}
		return printJSON(response)
	default:
		return fmt.Errorf("unknown nodes action %q", action)
	}
}

func runTasks(args []string) error {
	if len(args) == 0 {
		return errors.New("tasks requires submit, list, get, logs, wait, cancel, retry, or delete")
	}
	action := args[0]
	switch action {
	case "submit":
		return runTaskSubmit(args[1:])
	case "list":
		return runTaskList(args[1:])
	case "get":
		return runTaskGet(args[1:])
	case "logs":
		return runTaskLogs(args[1:])
	case "wait":
		return runTaskWait(args[1:])
	case "cancel":
		return runTaskCancel(args[1:])
	case "retry":
		return runTaskRetry(args[1:])
	case "delete":
		return runTaskDelete(args[1:])
	default:
		return fmt.Errorf("unknown tasks action %q", action)
	}
}

func runTaskSubmit(args []string) error {
	fs := flag.NewFlagSet("tasks submit", flag.ContinueOnError)
	options := addAdminFlags(fs)
	file := fs.String("file", "-", "task JSON file, or - for stdin")
	wait := fs.Bool("wait", false, "wait for terminal completion")
	follow := fs.Bool("follow", false, "stream logs while waiting")
	if err := parseInterspersed(fs, args); err != nil {
		return err
	}
	data, err := readInput(*file)
	if err != nil {
		return err
	}
	var spec core.TaskSpec
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&spec); err != nil {
		return fmt.Errorf("decode task JSON: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("decode task JSON: input must contain exactly one JSON value")
	}
	api, err := options.client()
	if err != nil {
		return err
	}
	var response struct {
		Task    core.Task `json:"task"`
		Created bool      `json:"created"`
	}
	if err := api.DoJSON(context.Background(), http.MethodPost, "/api/v1/tasks", nil, spec, &response, nil); err != nil {
		return err
	}
	fmt.Println(response.Task.ID)
	if *wait || *follow {
		return waitForTask(api, response.Task.ID, *follow, 0)
	}
	return nil
}

func runTaskList(args []string) error {
	fs := flag.NewFlagSet("tasks list", flag.ContinueOnError)
	options := addAdminFlags(fs)
	status := fs.String("status", "", "filter status")
	queue := fs.String("queue", "", "filter queue")
	nodeID := fs.String("node", "", "filter assigned node")
	limit := fs.Int("limit", 100, "maximum results")
	if err := parseInterspersed(fs, args); err != nil {
		return err
	}
	api, err := options.client()
	if err != nil {
		return err
	}
	query := url.Values{"limit": {strconv.Itoa(*limit)}}
	if *status != "" {
		query.Set("status", *status)
	}
	if *queue != "" {
		query.Set("queue", *queue)
	}
	if *nodeID != "" {
		query.Set("node_id", *nodeID)
	}
	var response any
	if err := api.DoJSON(context.Background(), http.MethodGet, "/api/v1/tasks", query, nil, &response, nil); err != nil {
		return err
	}
	return printJSON(response)
}

func runTaskGet(args []string) error {
	fs := flag.NewFlagSet("tasks get", flag.ContinueOnError)
	options := addAdminFlags(fs)
	if err := parseInterspersed(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("tasks get requires TASK_ID")
	}
	api, err := options.client()
	if err != nil {
		return err
	}
	var response any
	if err := api.DoJSON(context.Background(), http.MethodGet, "/api/v1/tasks/"+fs.Arg(0), nil, nil, &response, nil); err != nil {
		return err
	}
	return printJSON(response)
}

func runTaskLogs(args []string) error {
	fs := flag.NewFlagSet("tasks logs", flag.ContinueOnError)
	options := addAdminFlags(fs)
	follow := fs.Bool("follow", false, "continue polling until task is terminal")
	if err := parseInterspersed(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("tasks logs requires TASK_ID")
	}
	api, err := options.client()
	if err != nil {
		return err
	}
	if *follow {
		return waitForTask(api, fs.Arg(0), true, 0)
	}
	return printTaskLogsOnce(api, fs.Arg(0), 0)
}

func runTaskWait(args []string) error {
	fs := flag.NewFlagSet("tasks wait", flag.ContinueOnError)
	options := addAdminFlags(fs)
	follow := fs.Bool("follow", false, "stream logs")
	timeout := fs.Duration("wait-timeout", 0, "overall wait timeout; 0 means unlimited")
	if err := parseInterspersed(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("tasks wait requires TASK_ID")
	}
	api, err := options.client()
	if err != nil {
		return err
	}
	return waitForTask(api, fs.Arg(0), *follow, *timeout)
}

func runTaskCancel(args []string) error {
	fs := flag.NewFlagSet("tasks cancel", flag.ContinueOnError)
	options := addAdminFlags(fs)
	if err := parseInterspersed(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("tasks cancel requires TASK_ID")
	}
	api, err := options.client()
	if err != nil {
		return err
	}
	var response any
	if err := api.DoJSON(context.Background(), http.MethodPost, "/api/v1/tasks/"+fs.Arg(0)+"/cancel", nil, struct{}{}, &response, nil); err != nil {
		return err
	}
	return printJSON(response)
}

func runTaskRetry(args []string) error {
	fs := flag.NewFlagSet("tasks retry", flag.ContinueOnError)
	options := addAdminFlags(fs)
	reset := fs.Bool("reset-attempts", false, "reset attempt counter")
	ack := fs.Bool("acknowledge-non-idempotent", false, "explicitly authorize replay of a non-idempotent task")
	if err := parseInterspersed(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("tasks retry requires TASK_ID")
	}
	api, err := options.client()
	if err != nil {
		return err
	}
	request := map[string]bool{"reset_attempts": *reset, "acknowledge_non_idempotent": *ack}
	var response any
	if err := api.DoJSON(context.Background(), http.MethodPost, "/api/v1/tasks/"+fs.Arg(0)+"/retry", nil, request, &response, nil); err != nil {
		return err
	}
	return printJSON(response)
}

func runTaskDelete(args []string) error {
	fs := flag.NewFlagSet("tasks delete", flag.ContinueOnError)
	options := addAdminFlags(fs)
	if err := parseInterspersed(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("tasks delete requires TASK_ID")
	}
	api, err := options.client()
	if err != nil {
		return err
	}
	var response any
	if err := api.DoJSON(context.Background(), http.MethodDelete, "/api/v1/tasks/"+fs.Arg(0), nil, nil, &response, nil); err != nil {
		return err
	}
	return printJSON(response)
}

func runArtifacts(args []string) error {
	if len(args) == 0 {
		return errors.New("artifacts requires upload, list, get, download, delete, or gc")
	}
	action := args[0]
	fs := flag.NewFlagSet("artifacts "+action, flag.ContinueOnError)
	options := addAdminFlags(fs)
	name := fs.String("name", "", "artifact display name")
	mediaType := fs.String("media-type", "", "artifact media type")
	output := fs.String("output", "", "download destination")
	limit := fs.Int("limit", 100, "maximum list results")
	force := fs.Bool("force", false, "remove historical references before deleting an artifact")
	execute := fs.Bool("execute", false, "delete orphan files; GC is a dry run without this flag")
	if err := parseInterspersed(fs, args[1:]); err != nil {
		return err
	}
	api, err := options.client()
	if err != nil {
		return err
	}
	ctx := context.Background()
	switch action {
	case "upload":
		if fs.NArg() != 1 {
			return errors.New("artifacts upload requires FILE")
		}
		filePath := fs.Arg(0)
		query := url.Values{}
		if *name == "" {
			*name = filepath.Base(filePath)
		}
		query.Set("name", *name)
		if *mediaType != "" {
			query.Set("media_type", *mediaType)
		}
		var response any
		if err := api.UploadFile(ctx, "/api/v1/artifacts", query, filePath, map[string]string{"Content-Type": *mediaType}, &response); err != nil {
			return err
		}
		return printJSON(response)
	case "list":
		var response any
		if err := api.DoJSON(ctx, http.MethodGet, "/api/v1/artifacts", url.Values{"limit": {strconv.Itoa(*limit)}}, nil, &response, nil); err != nil {
			return err
		}
		return printJSON(response)
	case "get":
		if fs.NArg() != 1 {
			return errors.New("artifacts get requires ARTIFACT_ID")
		}
		var response any
		if err := api.DoJSON(ctx, http.MethodGet, "/api/v1/artifacts/"+fs.Arg(0), nil, nil, &response, nil); err != nil {
			return err
		}
		return printJSON(response)
	case "download":
		if fs.NArg() != 1 {
			return errors.New("artifacts download requires ARTIFACT_ID")
		}
		artifactID := fs.Arg(0)
		if *output == "" {
			*output = artifactID
		}
		if err := api.Download(ctx, "/api/v1/artifacts/"+artifactID+"/content", nil, *output, nil); err != nil {
			return err
		}
		fmt.Println(*output)
		return nil
	case "delete":
		if fs.NArg() != 1 {
			return errors.New("artifacts delete requires ARTIFACT_ID")
		}
		query := url.Values{}
		if *force {
			query.Set("force", "true")
		}
		var response any
		if err := api.DoJSON(ctx, http.MethodDelete, "/api/v1/artifacts/"+fs.Arg(0), query, nil, &response, nil); err != nil {
			return err
		}
		return printJSON(response)
	case "gc":
		if fs.NArg() != 0 {
			return errors.New("artifacts gc does not accept positional arguments")
		}
		var response any
		if err := api.DoJSON(ctx, http.MethodPost, "/api/v1/artifacts/gc", nil, map[string]bool{"delete": *execute}, &response, nil); err != nil {
			return err
		}
		return printJSON(response)
	default:
		return fmt.Errorf("unknown artifacts action %q", action)
	}
}

func runEvents(args []string) error {
	fs := flag.NewFlagSet("events", flag.ContinueOnError)
	options := addAdminFlags(fs)
	limit := fs.Int("limit", 100, "maximum events")
	typeFilter := fs.String("type", "", "event type filter")
	if err := parseInterspersed(fs, args); err != nil {
		return err
	}
	api, err := options.client()
	if err != nil {
		return err
	}
	query := url.Values{"limit": {strconv.Itoa(*limit)}}
	if *typeFilter != "" {
		query.Set("type", *typeFilter)
	}
	var response any
	if err := api.DoJSON(context.Background(), http.MethodGet, "/api/v1/events", query, nil, &response, nil); err != nil {
		return err
	}
	return printJSON(response)
}

func waitForTask(api *client.Client, taskID string, follow bool, timeout time.Duration) error {
	ctx := context.Background()
	var cancel context.CancelFunc
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	var after int64
	var currentExecutionID string
	for {
		if follow {
			query := url.Values{"after": {strconv.FormatInt(after, 10)}, "limit": {"5000"}}
			var logs struct {
				ExecutionID string `json:"execution_id"`
				Events      []struct {
					Sequence int64  `json:"sequence"`
					Stream   string `json:"stream"`
					Data     string `json:"data"`
				} `json:"events"`
				NextAfter int64 `json:"next_after"`
			}
			if err := api.DoJSON(ctx, http.MethodGet, "/api/v1/tasks/"+taskID+"/logs", query, nil, &logs, nil); err == nil {
				// Log sequence numbers are scoped to an execution. A retried task gets a
				// fresh execution, so reset the cursor rather than skipping its first lines.
				if logs.ExecutionID != "" && currentExecutionID != "" && logs.ExecutionID != currentExecutionID && after != 0 {
					after = 0
					currentExecutionID = logs.ExecutionID
					continue
				}
				if logs.ExecutionID != "" {
					currentExecutionID = logs.ExecutionID
				}
				for _, event := range logs.Events {
					writer := io.Writer(os.Stdout)
					if event.Stream == "stderr" {
						writer = os.Stderr
					}
					_, _ = io.WriteString(writer, event.Data)
				}
				after = logs.NextAfter
			}
		}
		var detail struct {
			Task core.Task `json:"task"`
		}
		if err := api.DoJSON(ctx, http.MethodGet, "/api/v1/tasks/"+taskID, nil, nil, &detail, nil); err != nil {
			return err
		}
		if core.IsTerminalTaskStatus(detail.Task.Status) {
			fmt.Fprintf(os.Stderr, "\n%s: %s\n", taskID, detail.Task.Status)
			if detail.Task.Status != core.TaskSucceeded {
				return fmt.Errorf("task finished with status %s: %s", detail.Task.Status, detail.Task.LastError)
			}
			return nil
		}
		if !sleepContext(ctx, time.Second) {
			return ctx.Err()
		}
	}
}

func printTaskLogsOnce(api *client.Client, taskID string, after int64) error {
	query := url.Values{"after": {strconv.FormatInt(after, 10)}, "limit": {"5000"}}
	var logs struct {
		Events []struct {
			Stream string `json:"stream"`
			Data   string `json:"data"`
		} `json:"events"`
	}
	if err := api.DoJSON(context.Background(), http.MethodGet, "/api/v1/tasks/"+taskID+"/logs", query, nil, &logs, nil); err != nil {
		return err
	}
	for _, event := range logs.Events {
		writer := io.Writer(os.Stdout)
		if event.Stream == "stderr" {
			writer = os.Stderr
		}
		_, _ = io.WriteString(writer, event.Data)
	}
	return nil
}

func readInput(path string) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(io.LimitReader(os.Stdin, 2<<20))
	}
	return os.ReadFile(path)
}

func printJSON(value any) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func newLogger(format, level string) *slog.Logger {
	var parsed slog.Level
	switch strings.ToLower(level) {
	case "debug":
		parsed = slog.LevelDebug
	case "warn", "warning":
		parsed = slog.LevelWarn
	case "error":
		parsed = slog.LevelError
	default:
		parsed = slog.LevelInfo
	}
	options := &slog.HandlerOptions{Level: parsed}
	if strings.EqualFold(format, "json") {
		return slog.New(slog.NewJSONHandler(os.Stdout, options))
	}
	return slog.New(slog.NewTextHandler(os.Stdout, options))
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func formatMap(values map[string]string) string {
	parts := make([]string, 0, len(values))
	for key, value := range values {
		parts = append(parts, key+"="+value)
	}
	return strings.Join(parts, ",")
}

func boolSetString(values map[string]bool) string {
	parts := []string{}
	for _, key := range []string{"shell", "process", "docker"} {
		if values[key] {
			parts = append(parts, key)
		}
	}
	return strings.Join(parts, ",")
}

func sleepContext(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
