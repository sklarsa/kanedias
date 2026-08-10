package cmd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sklarsa/kanedias/internal/config"
	"github.com/sklarsa/kanedias/internal/supervisor"
	"github.com/sklarsa/kanedias/internal/supervisor/contract"
	"github.com/sklarsa/kanedias/internal/supervisor/process"
	"github.com/sklarsa/kanedias/internal/supervisor/provision"
)

func validSupervisorConfig() config.Config {
	return config.Config{
		BaseImage: config.BaseImage{Name: "sandbox", Source: "images:", Image: "debian/13"},
		Models: map[string]config.ModelDefinition{
			"gpt-5-6-sol": {
				Label:                "GPT-5.6 Sol",
				Provider:             "openai-codex",
				Model:                "gpt-5.6-sol",
				ThinkingLevels:       []string{"low", "high"},
				DefaultThinkingLevel: "high",
			},
		},
		Session: config.SessionDefaults{ModelType: "gpt-5-6-sol", ThinkingLevel: "high"},
		Workers: map[string]config.WorkerDefaults{"worker": {
			Description: "work", ModelType: "gpt-5-6-sol",
		}},
	}
}

type recordingRootStatusWriter struct {
	bytes.Buffer
	closes int
}

func (writer *recordingRootStatusWriter) Close() error {
	writer.closes++
	return nil
}

func TestSessionStartupStatusReportsNodeStartOutcomeExactlyOnce(t *testing.T) {
	tests := []struct {
		name     string
		startErr error
		want     process.RootStartupStatus
	}{
		{name: "ready", want: process.RootStartupStatus{Status: process.RootStartupReady}},
		{name: "repository failure", startErr: errors.Join(contract.NewError(contract.ErrorWorkspaceRepositoryUnavailable, "public"), errors.New("private detail")), want: process.RootStartupStatus{Status: process.RootStartupFailure, Code: contract.ErrorWorkspaceRepositoryUnavailable}},
		{name: "internal failure", startErr: errors.New("plain failure"), want: process.RootStartupStatus{Status: process.RootStartupFailure, Code: contract.ErrorInternal}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			writer := &recordingRootStatusWriter{}
			if err := reportRootStartup(writer, test.startErr); err != nil {
				t.Fatal(err)
			}
			if writer.closes != 1 {
				t.Fatalf("status closes = %d, want 1", writer.closes)
			}
			got, err := process.DecodeRootStartupStatus(bytes.NewReader(writer.Bytes()))
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("status = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestRootWorkspaceStartReachesSupervisorDependencies(t *testing.T) {
	workspace := config.WorkspaceStart{Repository: "owner/repo", Checkout: "repo"}
	dependencies := rootSupervisorDependencies(SessionOptions{Workspace: workspace}, supervisor.Dependencies{})
	if dependencies.Workspace != workspace {
		t.Fatalf("dependencies workspace = %#v, want %#v", dependencies.Workspace, workspace)
	}
}

func TestRunSupervisorSelectsConfiguredEventLimitsBeforeProvisioning(t *testing.T) {
	maxEvents, maxBytes := 7, 1024
	cfg := validSupervisorConfig()
	cfg.Supervisor.Events = config.SupervisorEventsConfig{MaxEvents: &maxEvents, MaxBytes: &maxBytes}
	policy, err := cfg.DefaultSessionModelPolicy()
	if err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("broker sentinel")
	err = runSupervisorWithBrokerFactory(context.Background(), cfg, SessionOptions{
		SocketPath: filepath.Join(t.TempDir(), "root.sock"), ConfigPath: "/tmp/config.toml", Policy: policy,
	}, io.Discard, func(got supervisor.EventBrokerOptions) (*supervisor.EventBroker, error) {
		if got != (supervisor.EventBrokerOptions{MaxEvents: 7, MaxBytes: 1024}) {
			t.Fatalf("options = %#v", got)
		}
		return nil, sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v", err)
	}
}

func TestRunSupervisorDefaultEventLimitsWhenUnconfigured(t *testing.T) {
	cfg := validSupervisorConfig()
	policy, err := cfg.DefaultSessionModelPolicy()
	if err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("broker sentinel")
	err = runSupervisorWithBrokerFactory(context.Background(), cfg, SessionOptions{
		SocketPath: filepath.Join(t.TempDir(), "root.sock"), ConfigPath: "/tmp/config.toml", Policy: policy,
	}, io.Discard, func(got supervisor.EventBrokerOptions) (*supervisor.EventBroker, error) {
		want := supervisor.EventBrokerOptions{
			MaxEvents: config.DefaultSupervisorEventMaxEvents,
			MaxBytes:  config.DefaultSupervisorEventMaxBytes,
		}
		if got != want {
			t.Fatalf("options = %#v, want %#v", got, want)
		}
		return nil, sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v", err)
	}
}

func TestRunSupervisorRejectsInvalidWorkspaceStartBeforeConfigAndBroker(t *testing.T) {
	called := false
	policy, err := validSupervisorConfig().DefaultSessionModelPolicy()
	if err != nil {
		t.Fatal(err)
	}
	err = runSupervisorWithBrokerFactory(context.Background(), config.Config{}, SessionOptions{
		SocketPath: filepath.Join(t.TempDir(), "root.sock"), ConfigPath: "/tmp/config.toml", Policy: policy,
		Workspace: config.WorkspaceStart{Repository: "owner/repo", Checkout: "other"},
	}, io.Discard, func(supervisor.EventBrokerOptions) (*supervisor.EventBroker, error) {
		called = true
		return nil, nil
	})
	if err == nil || !strings.Contains(err.Error(), "workspace start") {
		t.Fatalf("workspace validation error = %v", err)
	}
	if called {
		t.Fatal("broker factory called before workspace validation")
	}
}

func TestRunSupervisorRejectsInvalidConfig(t *testing.T) {
	called := false
	policy, err := validSupervisorConfig().DefaultSessionModelPolicy()
	if err != nil {
		t.Fatal(err)
	}
	err = runSupervisorWithBrokerFactory(context.Background(), config.Config{}, SessionOptions{
		SocketPath: filepath.Join(t.TempDir(), "root.sock"), ConfigPath: "/tmp/config.toml", Policy: policy,
	}, io.Discard, func(supervisor.EventBrokerOptions) (*supervisor.EventBroker, error) {
		called = true
		return nil, nil
	})
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	if called {
		t.Fatal("broker factory called before config validation")
	}
}

func TestRunSupervisorRejectsInvalidPolicyBeforeConfigAndBroker(t *testing.T) {
	called := false
	err := runSupervisorWithBrokerFactory(context.Background(), config.Config{}, SessionOptions{
		SocketPath: filepath.Join(t.TempDir(), "root.sock"), ConfigPath: "/tmp/config.toml",
		Policy: config.SessionModelPolicy{Root: config.ModelProfile{Model: "model", ThinkingLevel: "high"}},
	}, io.Discard, func(supervisor.EventBrokerOptions) (*supervisor.EventBroker, error) {
		called = true
		return nil, nil
	})
	if err == nil || !strings.Contains(err.Error(), "provider") {
		t.Fatalf("policy validation error = %v", err)
	}
	if called {
		t.Fatal("broker factory called before policy validation")
	}
}

func TestInheritedChildPolicySelectsExactWorkerAndClonesMap(t *testing.T) {
	bootstrap := process.Bootstrap{
		Policy: config.SessionModelPolicy{
			Root: config.ModelProfile{Provider: "root-provider", Model: "root-model", ThinkingLevel: "off"},
			Workers: map[string]config.WorkerProfile{
				"worker":   {Description: "work", Provider: "admitted-provider", Model: "admitted-model", ThinkingLevel: "xhigh"},
				"reviewer": {Description: "review", Provider: "review-provider", Model: "review-model", ThinkingLevel: "medium"},
			},
		},
		Request: contract.CreateChildRequest{WorkerType: "worker"},
	}
	policy, worker, err := inheritedChildPolicy(bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	if want := bootstrap.Policy.Workers["worker"]; worker != want {
		t.Fatalf("selected worker = %#v, want exact inherited profile %#v", worker, want)
	}
	policy.Workers["worker"] = config.WorkerProfile{Description: "mutated", Provider: "mutated", Model: "mutated", ThinkingLevel: "off"}
	if reflect.DeepEqual(policy, bootstrap.Policy) {
		t.Fatal("resolved child policy aliases bootstrap policy")
	}
	if len(policy.Workers) != 2 || len(bootstrap.Policy.Workers) != 2 {
		t.Fatalf("worker roles lost: resolved=%#v bootstrap=%#v", policy.Workers, bootstrap.Policy.Workers)
	}
}

type recordingRuntimeChildProvisioner struct {
	request provision.ChildRequest
}

func (fake *recordingRuntimeChildProvisioner) ProvisionChild(_ context.Context, request provision.ChildRequest) (*provision.Resources, error) {
	fake.request = request
	return &provision.Resources{SessionID: request.SessionID, Pool: "pool", Instance: "session-" + request.SessionID, Volume: "workspace-" + request.SessionID, RPCAddr: "pipe"}, nil
}

func (*recordingRuntimeChildProvisioner) Destroy(context.Context, *provision.Resources) error {
	return nil
}

type runtimeTestRecoverer struct{}

func (runtimeTestRecoverer) RecoverDirectChild(context.Context, provision.RecoveryTicket) error {
	return nil
}

func TestProductionChildRunnerUsesInheritedPolicyDespiteChangedGlobalDefaults(t *testing.T) {
	content := `[network]
ipv4 = "10.76.111.1/24"
[base_image]
name = "sandbox"
source = "images:"
image = "debian/13"
[models.INVALID_NAME]
provider = ""
model = ""
thinking_levels = ["invalid"]
default_thinking_level = "missing"
[session]
model_type = "missing-current-default"
thinking_level = "invalid"
[workers.worker]
description = ""
model_type = "missing-current-worker-default"

[supervisor.events]
max_events = 7
max_bytes = 1024
`
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("KANEDIAS_CONFIG", path)

	sentinel := errors.New("broker sentinel")
	bootstrap := process.Bootstrap{
		SessionID: "session-child",
		ParentID:  "session-parent",
		RootID:    "session-root",
		Request: contract.CreateChildRequest{
			Kind:       contract.ChildKindRead,
			Context:    contract.ContextFresh,
			WorkerType: "worker",
			Task:       "test",
		},
		Policy: config.SessionModelPolicy{
			Root: config.ModelProfile{Provider: "admitted-root-provider", Model: "admitted-root-model", ThinkingLevel: "off"},
			Workers: map[string]config.WorkerProfile{
				"worker":   {Description: "admitted worker", Provider: "admitted-provider", Model: "admitted-model", ThinkingLevel: "xhigh"},
				"reviewer": {Description: "admitted reviewer", Provider: "review-provider", Model: "review-model", ThinkingLevel: "medium"},
			},
		},
		SourceInstance: "session-session-parent",
		SourceVolume:   "workspace-session-parent",
		SocketPath:     filepath.Join(t.TempDir(), "child.sock"),
	}

	// The sentinel is reached only after inherited-policy validation and worker
	// resolution, but before any provider/Incus side effect.
	err := productionChildRunnerWithBrokerFactory(context.Background(), bootstrap, nil, func(got supervisor.EventBrokerOptions) (*supervisor.EventBroker, error) {
		if got != (supervisor.EventBrokerOptions{MaxEvents: 7, MaxBytes: 1024}) {
			t.Fatalf("options = %#v", got)
		}
		return nil, sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v", err)
	}
}

func TestProductionChildWorkspaceStartRejectsInvalidBeforeConfig(t *testing.T) {
	t.Setenv("KANEDIAS_CONFIG", "")
	bootstrap := runtimePolicyBootstrap(t)
	bootstrap.Workspace = config.WorkspaceStart{Repository: "owner/repo", Checkout: "../repo"}
	err := productionChildRunnerWithRuntime(context.Background(), bootstrap, nil, supervisor.NewEventBrokerWithOptions, defaultProductionChildRuntime())
	if err == nil || !strings.Contains(err.Error(), "workspace start") {
		t.Fatalf("workspace validation error = %v", err)
	}
}

func TestProductionChildRunnerRejectsInvalidInfrastructureBeforeBroker(t *testing.T) {
	content := `[network]
ipv4 = "10.76.111.1/24"
[base_image]
source = "images:"
image = "debian/13"
[session]
model_type = "irrelevant"
[supervisor.events]
max_events = 7
max_bytes = 1024
`
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KANEDIAS_CONFIG", path)
	called := false
	bootstrap := runtimePolicyBootstrap(t)
	err := productionChildRunnerWithBrokerFactory(context.Background(), bootstrap, nil, func(supervisor.EventBrokerOptions) (*supervisor.EventBroker, error) {
		called = true
		return nil, errors.New("unexpected broker")
	})
	if err == nil || !strings.Contains(err.Error(), "base_image.name") {
		t.Fatalf("infrastructure validation error = %v", err)
	}
	if called {
		t.Fatal("broker created before infrastructure validation")
	}
}

func TestProductionChildRunnerRejectsInvalidRepositoryInfrastructure(t *testing.T) {
	content := `[network]
ipv4 = "10.76.111.1/24"
[base_image]
name = "sandbox"
source = "images:"
image = "debian/13"
[workspace]
repos = ["not-a-github-slug"]
[session]
model_type = "irrelevant"
`
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KANEDIAS_CONFIG", path)
	called := false
	err := productionChildRunnerWithBrokerFactory(context.Background(), runtimePolicyBootstrap(t), nil, func(supervisor.EventBrokerOptions) (*supervisor.EventBroker, error) {
		called = true
		return nil, errors.New("unexpected broker")
	})
	if err == nil || !strings.Contains(err.Error(), "trusted workspace repository") {
		t.Fatalf("repository validation error = %v", err)
	}
	if called {
		t.Fatal("broker created before repository validation")
	}
}

func TestProductionChildWorkspaceInheritanceIgnoresChangedRepositoryDefaults(t *testing.T) {
	content := `[network]
ipv4 = "10.76.111.1/24"
[base_image]
name = "sandbox"
source = "images:"
image = "debian/13"
[workspace]
repos = ["changed/default"]
[models.INVALID_NAME]
provider = ""
model = ""
thinking_levels = ["invalid"]
default_thinking_level = "missing"
[session]
model_type = "missing-current-default"
thinking_level = "invalid"
[workers.worker]
description = ""
model_type = "missing-current-worker-default"
`
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KANEDIAS_CONFIG", path)
	bootstrap := runtimePolicyBootstrap(t)
	bootstrap.Workspace = config.WorkspaceStart{Repository: "owner/repo", Checkout: "repo"}
	originalPolicy := bootstrap.Policy.Clone()
	originalWorkspace := bootstrap.Workspace
	provisioner := &recordingRuntimeChildProvisioner{}
	var nested []process.Bootstrap

	runtime := defaultProductionChildRuntime()
	runtime.newChildProvisioner = func(context.Context, config.Config) (provision.ChildProvisioner, func(), error) {
		return provisioner, func() {}, nil
	}
	runtime.newDirectChildRecoverer = func(context.Context, config.Config) (provision.DirectChildRecoverer, func(), error) {
		return runtimeTestRecoverer{}, func() {}, nil
	}
	runtime.dialRPC = func(context.Context, string) (io.ReadWriteCloser, error) {
		host, peer := net.Pipe()
		go serveRuntimePolicyState(peer, bootstrap.Policy.Workers[bootstrap.Request.WorkerType])
		return host, nil
	}
	runtime.spawnChild = func(string) supervisor.ChildSpawner {
		return func(_ context.Context, got process.Bootstrap) (supervisor.ChildProcess, error) {
			nested = append(nested, got)
			return nil, errors.New("captured nested bootstrap")
		}
	}
	runtime.descendantClient = func(string) (supervisor.DescendantClient, error) {
		return nil, errors.New("unexpected descendant client")
	}
	sentinel := errors.New("nested policy captured")
	runtime.afterReady = func(ctx context.Context, node *supervisor.Node) error {
		request := contract.CreateChildRequest{WorkerType: "reviewer", Kind: contract.ChildKindRead, Context: contract.ContextFresh, Task: "nested review"}
		_, _ = node.CreateChild(ctx, bootstrap.SessionID, request)
		if len(nested) != 1 {
			t.Fatalf("nested spawn count = %d, want 1", len(nested))
		}
		if !reflect.DeepEqual(nested[0].Policy, originalPolicy) {
			t.Fatalf("nested policy = %#v, want original %#v", nested[0].Policy, originalPolicy)
		}
		if nested[0].Workspace != originalWorkspace {
			t.Fatalf("nested workspace = %#v, want inherited %#v", nested[0].Workspace, originalWorkspace)
		}
		mutated := nested[0].Policy.Workers["reviewer"]
		mutated.Provider, mutated.Model = "mutated-provider", "mutated-model"
		nested[0].Policy.Workers["reviewer"] = mutated
		nested[0].Workspace = config.WorkspaceStart{Repository: "changed/default", Checkout: "default"}
		_, _ = node.CreateChild(ctx, bootstrap.SessionID, request)
		if len(nested) != 2 || !reflect.DeepEqual(nested[1].Policy, originalPolicy) {
			t.Fatalf("second nested policy = %#v, want non-aliased original %#v", nested, originalPolicy)
		}
		return sentinel
	}
	reporter := process.NewAcknowledgedReporter(context.Background(), io.Discard, nil, bootstrap.SessionID)
	err := productionChildRunnerWithRuntime(context.Background(), bootstrap, reporter, supervisor.NewEventBrokerWithOptions, runtime)
	if !errors.Is(err, sentinel) {
		t.Fatalf("runner error = %v, want sentinel", err)
	}
	if got, want := provisioner.request.Worker, originalPolicy.Workers[bootstrap.Request.WorkerType]; got != want {
		t.Fatalf("provisioned worker = %#v, want inherited %#v", got, want)
	}
	if got := provisioner.request.Workspace; got != originalWorkspace {
		t.Fatalf("child provision workspace = %#v, want inherited %#v", got, originalWorkspace)
	}
	if len(nested) != 2 || nested[1].Workspace != originalWorkspace {
		t.Fatalf("grandchild workspace = %#v, want immutable inherited %#v", nested, originalWorkspace)
	}
	if len(nested[1].Policy.Workers) != len(originalPolicy.Workers) {
		t.Fatalf("nested workers = %#v, want every role %#v", nested[1].Policy.Workers, originalPolicy.Workers)
	}
}

func TestProductionReadFailurePublishesTypedMessageAndWaitsForAcknowledgement(t *testing.T) {
	report := &recordingWriteCloser{onWrite: make(chan struct{}, 1)}
	ackReport, ackAck := net.Pipe()
	reporter := process.NewAcknowledgedReporter(context.Background(), report, ackAck, "session-child")

	done := make(chan error, 1)
	go func() {
		done <- publishReadFailure(context.Background(), reporter,
			contract.NewError(contract.ErrorChildAborted, "child was stopped"))
	}()

	select {
	case <-report.onWrite:
	case <-time.After(time.Second):
		t.Fatal("typed failure was not published")
	}
	// The helper must remain blocked awaiting the parent acknowledgement before
	// returning, so the runtime's deferred node teardown cannot precede ingestion.
	select {
	case err := <-done:
		t.Fatalf("publishReadFailure returned before acknowledgement: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if _, err := ackReport.Write([]byte{process.TerminalAckByte}); err != nil {
		t.Fatal(err)
	}
	_ = ackReport.Close()

	var msg process.ChildMessage
	if err := json.Unmarshal([]byte(report.String()), &msg); err != nil {
		t.Fatalf("decode published failure: %v", err)
	}
	if msg.Type != process.MessageFailure || msg.Error == nil ||
		msg.Error.Code != contract.ErrorChildAborted || msg.Error.Message != "child was stopped" ||
		msg.SessionID != "session-child" {
		t.Fatalf("published failure = %#v", msg)
	}
	if err := <-done; err == nil {
		t.Fatal("publishReadFailure returned no error for a failed read")
	}
	if !reporter.TerminalSent() {
		t.Fatal("terminal failure was not marked sent")
	}
}

func TestProductionReadFailureMapsUntypedErrorToInternal(t *testing.T) {
	report := &recordingWriteCloser{}
	ackReport, ackAck := net.Pipe()
	reporter := process.NewAcknowledgedReporter(context.Background(), report, ackAck, "session-child")
	done := make(chan error, 1)
	go func() {
		done <- publishReadFailure(context.Background(), reporter, errors.New("boom"))
	}()
	if _, err := ackReport.Write([]byte{process.TerminalAckByte}); err != nil {
		t.Fatal(err)
	}
	_ = ackReport.Close()
	if err := <-done; err == nil {
		t.Fatal("publishReadFailure returned no joined error")
	}
	var msg process.ChildMessage
	if err := json.Unmarshal([]byte(report.String()), &msg); err != nil {
		t.Fatalf("decode published failure: %v", err)
	}
	if msg.Error == nil || msg.Error.Code != contract.ErrorInternal || msg.Error.Message != "internal supervisor error" {
		t.Fatalf("untyped failure mapped to %#v", msg.Error)
	}
}

func TestInheritedCancellationPublishesNoTerminalFailure(t *testing.T) {
	report := &recordingWriteCloser{}
	ackReport, ackAck := net.Pipe()
	defer func() { _ = ackAck.Close() }()
	defer func() { _ = ackReport.Close() }()
	reporter := process.NewAcknowledgedReporter(context.Background(), report, ackAck, "session-child")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := publishReadFailure(ctx, reporter, contract.NewError(contract.ErrorChildAborted, "child was stopped"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("publishReadFailure returned %v, want context.Canceled under inherited cancellation", err)
	}
	if reporter.TerminalSent() {
		t.Fatal("terminal failure published under inherited cancellation")
	}
	if report.String() != "" {
		t.Fatalf("terminal report written under inherited cancellation: %q", report.String())
	}
}

// TestProductionReadFailureDrainsAdmittedRPCBeforePublishing proves the Task 6L
// ordering seam: an admitted abort-like Node.CallRPC stays blocked while the
// read task observes aborted settlement, and no terminal failure is published
// until that admitted RPC response is released. Only after the admitted RPC
// returns exact success is the single typed child_aborted failure published,
// acknowledged by the direct parent, and only then does runtime teardown run.
func TestProductionReadFailureDrainsAdmittedRPCBeforePublishing(t *testing.T) {
	report := &recordingWriteCloser{onWrite: make(chan struct{}, 1)}
	ackReport, ackAck := net.Pipe()
	defer func() { _ = ackReport.Close() }()
	defer func() { _ = ackAck.Close() }()
	reporter := process.NewAcknowledgedReporter(context.Background(), report, ackAck, "session-child")

	configPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(configPath, []byte(validChildRuntimeConfig()), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KANEDIAS_CONFIG", configPath)

	bootstrap := runtimePolicyBootstrap(t)
	bootstrap.SocketPath = filepath.Join(t.TempDir(), "child.sock")

	abortReceived := make(chan struct{})
	releaseAbort := make(chan struct{})
	promptAcked := make(chan struct{})
	emitSettlement := make(chan struct{})
	settlementWritten := make(chan struct{})
	serverDone := make(chan struct{})
	abortReturned := make(chan struct{})

	runtime := defaultProductionChildRuntime()
	runtime.newChildProvisioner = func(context.Context, config.Config) (provision.ChildProvisioner, func(), error) {
		return &recordingRuntimeChildProvisioner{}, func() {}, nil
	}
	runtime.newDirectChildRecoverer = func(context.Context, config.Config) (provision.DirectChildRecoverer, func(), error) {
		return runtimeTestRecoverer{}, func() {}, nil
	}
	runtime.spawnChild = func(string) supervisor.ChildSpawner {
		return func(context.Context, process.Bootstrap) (supervisor.ChildProcess, error) {
			return nil, errors.New("unexpected child spawn")
		}
	}
	runtime.descendantClient = func(string) (supervisor.DescendantClient, error) {
		return nil, errors.New("unexpected descendant client")
	}

	// afterReady runs before RunReadTask, so the abort-like RPC is already
	// admitted (and its response held) while the read task is running.
	runtime.afterReady = func(_ context.Context, node *supervisor.Node) error {
		go func() {
			_, _ = node.CallRPC(context.Background(), json.RawMessage(`{"type":"abort"}`))
			close(abortReturned)
		}()
		return nil
	}

	host, peer := net.Pipe()
	runtime.dialRPC = func(context.Context, string) (io.ReadWriteCloser, error) { return host, nil }

	go func() {
		defer close(serverDone)
		reader := bufio.NewReader(peer)
		for {
			line, err := reader.ReadBytes('\n')
			if err != nil {
				return
			}
			var command struct {
				ID   string `json:"id"`
				Type string `json:"type"`
			}
			if json.Unmarshal(line, &command) != nil {
				continue
			}
			switch command.Type {
			case "get_state":
				_ = writeRuntimeResponse(peer, command.ID, "get_state", true, map[string]any{
					"sessionId": "pi-child", "sessionFile": "/tmp/pi-child.jsonl", "isStreaming": false,
					"model":         map[string]any{"provider": "admitted-provider", "id": "admitted-model"},
					"thinkingLevel": "xhigh",
				})
			case "prompt":
				if err := writeRuntimeResponse(peer, command.ID, "prompt", true, nil); err != nil {
					return
				}
				close(promptAcked)
			case "abort":
				close(abortReceived)
				go func(id string) {
					<-releaseAbort
					_ = writeRuntimeResponse(peer, id, "abort", true, nil)
				}(command.ID)
			}
		}
	}()

	// Emit the aborted settlement only when the test is ready to observe it; the
	// events are written to the transport so RunReadTask settles.
	go func() {
		defer close(settlementWritten)
		<-emitSettlement
		_, _ = peer.Write([]byte(`{"type":"message_end","message":{"role":"assistant","stopReason":"aborted"}}` + "\n"))
		_, _ = peer.Write([]byte(`{"type":"agent_settled"}` + "\n"))
	}()

	runnerDone := make(chan error, 1)
	go func() {
		runnerDone <- productionChildRunnerWithRuntime(context.Background(), bootstrap, reporter, supervisor.NewEventBrokerWithOptions, runtime)
	}()

	select {
	case <-abortReceived:
	case err := <-runnerDone:
		t.Fatalf("runner exited before abort was admitted: %v", err)
	}
	<-promptAcked
	close(emitSettlement)
	<-settlementWritten

	// RunReadTask observes the aborted settlement and the runtime reaches the
	// drain boundary. While the admitted abort RPC is still held, no terminal
	// failure may be published: the runtime stays blocked in QuiesceRPC.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !reporter.TerminalSent() {
		time.Sleep(time.Millisecond)
	}
	if reporter.TerminalSent() {
		t.Fatalf("terminal failure was published before the admitted abort RPC was released")
	}

	// Release the admitted abort RPC: it returns exact success, the drain
	// completes, and exactly one typed child_aborted failure is published.
	close(releaseAbort)
	select {
	case <-abortReturned:
	case <-time.After(time.Second):
		t.Fatal("admitted abort RPC did not return after release")
	}
	testWaitFor(t, func() bool { return reporter.TerminalSent() }, "typed child_aborted failure after drain")

	var terminal *process.ChildMessage
	failureCount := 0
	dec := json.NewDecoder(bytes.NewReader([]byte(report.String())))
	for {
		var m process.ChildMessage
		if err := dec.Decode(&m); err != nil {
			break
		}
		if m.Type == process.MessageFailure {
			failureCount++
			terminal = &m
		}
	}
	if terminal == nil || failureCount != 1 || terminal.Error == nil ||
		terminal.Error.Code != contract.ErrorChildAborted || terminal.Error.Message != "read child was aborted" {
		t.Fatalf("published terminal failure = %#v (failures=%d, report %q)", terminal, failureCount, report.String())
	}

	// Parent acknowledgement unblocks the report; only then does the deferred
	// node teardown run and the runner return.
	if _, err := ackReport.Write([]byte{process.TerminalAckByte}); err != nil {
		t.Fatal(err)
	}
	_ = ackReport.Close()
	select {
	case err := <-runnerDone:
		// The runner returns only after the acknowledged teardown, carrying the
		// exact typed read failure (the child runtime's natural exit error).
		var typed *contract.Error
		if !errors.As(err, &typed) || typed.Code != contract.ErrorChildAborted {
			t.Fatalf("runner error = %v, want child_aborted after acknowledgement", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runner did not finish after terminal acknowledgement")
	}
}

func validChildRuntimeConfig() string {
	return `[network]
ipv4 = "10.76.111.1/24"
[base_image]
name = "sandbox"
source = "images:"
image = "debian/13"
[session]
model_type = "irrelevant"
[supervisor.events]
max_events = 7
max_bytes = 1024
`
}

func writeRuntimeResponse(peer net.Conn, id, command string, success bool, data any) error {
	value := map[string]any{"id": id, "type": "response", "command": command, "success": success}
	if data != nil {
		value["data"] = data
	}
	wire, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = peer.Write(append(wire, '\n'))
	return err
}

func testWaitFor(t *testing.T, condition func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

// recordingWriteCloser captures report bytes and optionally signals once per write.
type recordingWriteCloser struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	onWrite chan struct{}
}

func (w *recordingWriteCloser) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	if w.onWrite != nil {
		select {
		case w.onWrite <- struct{}{}:
		default:
		}
	}
	return len(p), nil
}

func (w *recordingWriteCloser) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func runtimePolicyBootstrap(t *testing.T) process.Bootstrap {
	t.Helper()
	return process.Bootstrap{
		SessionID: "session-child", ParentID: "session-parent", RootID: "session-root",
		SocketPath: filepath.Join(t.TempDir(), "child.sock"), SourceInstance: "session-parent", SourceVolume: "workspace-parent",
		Policy: config.SessionModelPolicy{
			Root: config.ModelProfile{Provider: "root-provider", Model: "root-model", ThinkingLevel: "off"},
			Workers: map[string]config.WorkerProfile{
				"worker":   {Description: "work", Provider: "admitted-provider", Model: "admitted-model", ThinkingLevel: "xhigh"},
				"reviewer": {Description: "review", Provider: "review-provider", Model: "review-model", ThinkingLevel: "medium"},
			},
		},
		Request: contract.CreateChildRequest{WorkerType: "worker", Kind: contract.ChildKindRead, Context: contract.ContextFresh, Task: "parent task"},
	}
}

func serveRuntimePolicyState(peer net.Conn, worker config.WorkerProfile) {
	defer func() { _ = peer.Close() }()
	line, err := bufio.NewReader(peer).ReadBytes('\n')
	if err != nil {
		return
	}
	var command struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(line, &command) != nil {
		return
	}
	response, _ := json.Marshal(map[string]any{
		"id": command.ID, "type": "response", "command": "get_state", "success": true,
		"data": map[string]any{
			"sessionId": "pi-child", "sessionFile": "/tmp/pi-child.jsonl", "isStreaming": false,
			"model": map[string]any{"provider": worker.Provider, "id": worker.Model}, "thinkingLevel": worker.ThinkingLevel,
		},
	})
	_, _ = peer.Write(append(response, '\n'))
	_, _ = io.Copy(io.Discard, peer)
}
