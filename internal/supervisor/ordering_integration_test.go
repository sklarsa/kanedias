//go:build unix

package supervisor_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sklarsa/kanedias/internal/config"
	"github.com/sklarsa/kanedias/internal/supervisor"
	"github.com/sklarsa/kanedias/internal/supervisor/contract"
	"github.com/sklarsa/kanedias/internal/supervisor/process"
	"github.com/sklarsa/kanedias/internal/supervisor/provision"
	"github.com/sklarsa/kanedias/internal/supervisorapi"
)

const orderingHelperEnv = "KANEDIAS_ORDERING_HELPER"

type orderingWorkers struct{}

func (orderingWorkers) Resolve(name string) (config.WorkerProfile, error) {
	if name != "worker" {
		return config.WorkerProfile{}, contract.NewError(contract.ErrorUnknownWorkerType, "unknown worker")
	}
	return config.WorkerProfile{Description: "Write code", Provider: "test", Model: "test-model"}, nil
}
func (orderingWorkers) Summaries() []contract.WorkerSummary { return nil }

type orderingRootProvisioner struct{}

func (orderingRootProvisioner) ProvisionRoot(context.Context, provision.RootRequest) (*provision.Resources, error) {
	return &provision.Resources{SessionID: "root-order", Pool: "pool", Instance: "root-instance", Volume: "root-volume", RPCAddr: "pipe"}, nil
}
func (orderingRootProvisioner) Destroy(context.Context, *provision.Resources) error { return nil }

type orderingRecoverer struct{ path string }

func (recoverer orderingRecoverer) RecoverDirectChild(context.Context, provision.RecoveryTicket) error {
	return appendOrderingStep(recoverer.path, "recovery")
}

type heldTerminalChild struct {
	*process.Child
	held    chan struct{}
	release <-chan struct{}
}

func (child *heldTerminalChild) NextMessage(ctx context.Context) (process.ChildMessage, error) {
	message, err := child.Child.NextMessage(ctx)
	if err != nil || (message.Type != process.MessageRead && message.Type != process.MessageWrite && message.Type != process.MessageFailure) {
		return message, err
	}
	close(child.held)
	select {
	case <-child.release:
		return message, nil
	case <-ctx.Done():
		return process.ChildMessage{}, ctx.Err()
	}
}

func TestIntegratedHandoffOrderingThroughSpawnerReportPipeAndHTTPFlush(t *testing.T) {
	if os.Getenv(orderingHelperEnv) == "ordering" {
		runOrderingChildHelper(t)
		return
	}

	orderPath := filepath.Join(t.TempDir(), "order.log")
	t.Setenv(orderingHelperEnv, "ordering")
	t.Setenv("KANEDIAS_ORDERING_FILE", orderPath)
	t.Setenv("KANEDIAS_ORDERING_TEST_BINARY", os.Args[0])
	script := filepath.Join(t.TempDir(), "child-helper.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec \"$KANEDIAS_ORDERING_TEST_BINARY\" -test.run '^TestIntegratedHandoffOrderingThroughSpawnerReportPipeAndHTTPFlush$'\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	rootSocket := filepath.Join(t.TempDir(), "root.sock")
	listener, err := net.Listen("unix", rootSocket)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(rootSocket, 0o600); err != nil {
		t.Fatal(err)
	}
	clientConn, piPeer := net.Pipe()
	piDone := startOrderingPiPeer(piPeer)
	identity, err := supervisor.NewIdentity(supervisor.IdentitySpec{SessionID: "root-order", RootID: "root-order", Kind: contract.ChildKindRoot, Context: contract.ContextRoot})
	if err != nil {
		t.Fatal(err)
	}
	spawner := process.Spawner{Executable: script, ProbeInterval: time.Millisecond}
	terminalHeld := make(chan struct{})
	releaseTerminal := make(chan struct{})
	var spawnedChild *heldTerminalChild
	var childSocket string
	node, err := supervisor.NewRoot(identity, supervisor.Dependencies{
		Provisioner: orderingRootProvisioner{},
		DialRPC:     func(context.Context, string) (io.ReadWriteCloser, error) { return clientConn, nil },
		Workers:     orderingWorkers{}, SocketPath: rootSocket,
		SpawnChild: func(ctx context.Context, bootstrap process.Bootstrap) (supervisor.ChildProcess, error) {
			child, err := spawner.Spawn(ctx, bootstrap)
			if err != nil {
				return nil, err
			}
			childSocket = bootstrap.SocketPath
			spawnedChild = &heldTerminalChild{Child: child, held: terminalHeld, release: releaseTerminal}
			return spawnedChild, nil
		},
		DescendantClient:     supervisorapi.NewDescendantClient,
		DirectChildRecoverer: orderingRecoverer{path: orderPath},
		NewSessionID:         func() (string, error) { return "writer-order", nil },
		ChildStopTimeout:     2 * time.Second,
		CloseListener:        func(context.Context) error { return listener.Close() },
	}, supervisor.NewEventBroker())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := node.Start(ctx); err != nil {
		t.Fatal(err)
	}

	returned := make(chan struct {
		result supervisor.TerminalResult
		err    error
	}, 1)
	go func() {
		result, err := node.CreateChild(ctx, "root-order", contract.CreateChildRequest{WorkerType: "worker", Kind: contract.ChildKindWrite, Context: contract.ContextFresh, Task: "persist, verify, and hand off"})
		returned <- struct {
			result supervisor.TerminalResult
			err    error
		}{result, err}
	}()

	select {
	case <-terminalHeld:
	case <-ctx.Done():
		t.Fatalf("parent did not reach held terminal ingestion: %v", ctx.Err())
	}
	// Hold direct-parent terminal ingestion longer than the rejected 250 ms
	// grace. The child is blocked in Reporter.Write, so its process and HTTP/SSE
	// server must remain live until this exact report is released and acked.
	select {
	case <-spawnedChild.Done():
		t.Fatal("child process exited before terminal report acknowledgement")
	case <-time.After(350 * time.Millisecond):
	}
	probe, err := supervisorapi.NewDescendantClient(childSocket)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := probe.Snapshot(ctx); err != nil {
		t.Fatalf("child server tore down before terminal acknowledgement: %v", err)
	}
	subscription, err := probe.Subscribe(ctx)
	if err != nil {
		t.Fatalf("child SSE tore down before terminal acknowledgement: %v", err)
	}
	select {
	case _, ok := <-subscription.Events:
		if !ok {
			t.Fatal("child SSE closed before terminal acknowledgement")
		}
	case <-time.After(20 * time.Millisecond):
	}
	if subscription.Close != nil {
		subscription.Close()
	}
	if closer, ok := probe.(io.Closer); ok {
		_ = closer.Close()
	}
	if steps := readOrderingSteps(orderPath); !reflect.DeepEqual(steps, []string{"verified"}) {
		t.Fatalf("pre-ack order = %v, want [verified]", steps)
	}
	close(releaseTerminal)

	var got struct {
		result supervisor.TerminalResult
		err    error
	}
	select {
	case got = <-returned:
	case <-ctx.Done():
		t.Fatalf("blocked parent CreateChild did not return: %v; order=%v", ctx.Err(), readOrderingSteps(orderPath))
	}
	if got.err != nil {
		t.Fatalf("CreateChild: %v; order=%v", got.err, readOrderingSteps(orderPath))
	}
	if got.result.Write == nil || len(got.result.Write.Repositories) != 1 || got.result.Write.Repositories[0].HeadCommit != strings.Repeat("a", 40) {
		t.Fatalf("forwarded write result = %#v", got.result.Write)
	}
	if err := appendOrderingStep(orderPath, "return"); err != nil {
		t.Fatal(err)
	}
	want := []string{"verified", "report", "acceptance", "teardown", "exit", "recovery", "return"}
	if steps := readOrderingSteps(orderPath); !reflect.DeepEqual(steps, want) {
		t.Fatalf("integrated order = %v, want %v", steps, want)
	}

	stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
	defer stopCancel()
	if err := node.Stop(stopCtx, supervisor.StopReasonRequested); err != nil {
		t.Fatal(err)
	}
	select {
	case <-piDone:
	case <-time.After(time.Second):
		t.Fatal("root Pi peer did not observe cleanup")
	}
}

func TestUnexpectedCleanDescendantSSEEOFFailsAndCleansOwnedChild(t *testing.T) {
	if os.Getenv(orderingHelperEnv) == "stream-failure" {
		runCleanEOFChildHelper(t)
		return
	}

	orderPath := filepath.Join(t.TempDir(), "stream-order.log")
	t.Setenv(orderingHelperEnv, "stream-failure")
	t.Setenv("KANEDIAS_ORDERING_FILE", orderPath)
	t.Setenv("KANEDIAS_ORDERING_TEST_BINARY", os.Args[0])
	script := filepath.Join(t.TempDir(), "stream-helper.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec \"$KANEDIAS_ORDERING_TEST_BINARY\" -test.run '^TestUnexpectedCleanDescendantSSEEOFFailsAndCleansOwnedChild$'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	rootSocket := filepath.Join(t.TempDir(), "root.sock")
	listener, err := net.Listen("unix", rootSocket)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(rootSocket, 0o600); err != nil {
		t.Fatal(err)
	}
	clientConn, piPeer := net.Pipe()
	piDone := startOrderingPiPeer(piPeer)
	identity, err := supervisor.NewIdentity(supervisor.IdentitySpec{SessionID: "root-order", RootID: "root-order", Kind: contract.ChildKindRoot, Context: contract.ContextRoot})
	if err != nil {
		t.Fatal(err)
	}
	spawner := process.Spawner{Executable: script, ProbeInterval: time.Millisecond}
	node, err := supervisor.NewRoot(identity, supervisor.Dependencies{
		Provisioner: orderingRootProvisioner{},
		DialRPC:     func(context.Context, string) (io.ReadWriteCloser, error) { return clientConn, nil },
		Workers:     orderingWorkers{}, SocketPath: rootSocket,
		SpawnChild: func(ctx context.Context, bootstrap process.Bootstrap) (supervisor.ChildProcess, error) {
			return spawner.Spawn(ctx, bootstrap)
		},
		DescendantClient: supervisorapi.NewDescendantClient, DirectChildRecoverer: orderingRecoverer{path: orderPath},
		NewSessionID: func() (string, error) { return "writer-stream", nil }, ChildStopTimeout: 2 * time.Second,
		CloseListener: func(context.Context) error { return listener.Close() },
	}, supervisor.NewEventBroker())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := node.Start(ctx); err != nil {
		t.Fatal(err)
	}
	startedFailureWait := time.Now()
	_, err = node.CreateChild(ctx, "root-order", contract.CreateChildRequest{WorkerType: "worker", Kind: contract.ChildKindWrite, Context: contract.ContextFresh, Task: "remain active until stream ownership fails"})
	if elapsed := time.Since(startedFailureWait); elapsed >= 2*time.Second {
		t.Fatalf("unexpected SSE EOF waited for a possible terminal report for %s", elapsed)
	}
	var typed *contract.Error
	if !errors.As(err, &typed) || typed.Code != contract.ErrorChildFailed {
		t.Fatalf("clean descendant SSE EOF error = %v, want typed child_failed", err)
	}
	steps := readOrderingSteps(orderPath)
	if !reflect.DeepEqual(steps, []string{"liveness-closed", "recovery"}) {
		t.Fatalf("clean SSE failure cleanup order = %v", steps)
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
	defer stopCancel()
	if err := node.Stop(stopCtx, supervisor.StopReasonRequested); err != nil {
		t.Fatal(err)
	}
	select {
	case <-piDone:
	case <-time.After(time.Second):
		t.Fatal("root Pi peer did not observe cleanup")
	}
}

func startOrderingPiPeer(peer net.Conn) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { _ = peer.Close() }()
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
				return
			}
			response := map[string]any{"id": command.ID, "type": "response", "command": command.Type, "success": true}
			if command.Type == "get_state" {
				response["data"] = map[string]any{"sessionId": "pi-root-order", "sessionFile": "/tmp/root-order.jsonl", "isStreaming": false}
			}
			wire, _ := json.Marshal(response)
			if _, err := peer.Write(append(wire, '\n')); err != nil {
				return
			}
		}
	}()
	return done
}

type orderingFakeChildProvisioner struct{ orderPath string }

func (orderingFakeChildProvisioner) ProvisionChild(context.Context, provision.ChildRequest) (*provision.Resources, error) {
	return nil, errors.New("unused")
}
func (provisioner orderingFakeChildProvisioner) Destroy(context.Context, *provision.Resources) error {
	return appendOrderingStep(provisioner.orderPath, "teardown")
}

type orderingChildService struct {
	bootstrap   process.Bootstrap
	reporter    *process.Reporter
	provisioner provision.ChildProvisioner
	orderPath   string
	events      chan supervisor.EventEnvelope
	observed    chan struct{}
	ready       chan struct{}
	readyOnce   sync.Once
}

func (service *orderingChildService) Snapshot(context.Context) (supervisor.NodeSnapshot, error) {
	service.readyOnce.Do(func() { close(service.ready) })
	return supervisor.NodeSnapshot{SessionID: service.bootstrap.SessionID, ParentSessionID: service.bootstrap.ParentID, RootSessionID: service.bootstrap.RootID, Kind: service.bootstrap.Request.Kind, Context: service.bootstrap.Request.Context, WorkerType: service.bootstrap.Request.WorkerType, Lifecycle: "ready", Questions: []supervisor.QuestionSummary{}, Children: []supervisor.NodeSnapshot{}}, nil
}
func (*orderingChildService) Workers(context.Context) []contract.WorkerSummary { return nil }
func (*orderingChildService) CallRPC(context.Context, string, json.RawMessage) (json.RawMessage, error) {
	return nil, errors.New("unused")
}
func (*orderingChildService) AnswerQuestion(context.Context, string, string, json.RawMessage) error {
	return errors.New("unused")
}
func (service *orderingChildService) Subscribe(context.Context) (supervisor.Subscription, error) {
	return supervisor.Subscription{Replay: []supervisor.EventEnvelope{}, Events: service.events, Close: func() {}}, nil
}
func (*orderingChildService) CreateChild(context.Context, string, contract.CreateChildRequest) (supervisor.TerminalResult, error) {
	return supervisor.TerminalResult{}, errors.New("unused")
}
func (service *orderingChildService) Handoff(_ context.Context, request supervisor.WriteHandoffRequest) (supervisor.HandoffAcceptance, error) {
	if len(request.Repositories) != 1 || request.Repositories[0].Repository != "owner/disposable" || request.Repositories[0].HeadCommit != strings.Repeat("a", 40) {
		return supervisor.HandoffAcceptance{}, errors.New("unverified handoff")
	}
	if err := appendOrderingStep(service.orderPath, "verified"); err != nil {
		return supervisor.HandoffAcceptance{}, err
	}
	result := contract.WriteChildResult{Kind: contract.ChildKindWrite, WorkerType: service.bootstrap.Request.WorkerType, SessionID: service.bootstrap.SessionID, Repositories: request.Repositories, Summary: request.Summary, Verification: request.Verification}
	if err := service.reporter.Write(result); err != nil {
		return supervisor.HandoffAcceptance{}, err
	}
	if err := appendOrderingStep(service.orderPath, "report"); err != nil {
		return supervisor.HandoffAcceptance{}, err
	}
	return supervisor.HandoffAcceptance{Accepted: true, SessionID: service.bootstrap.SessionID}, nil
}
func (service *orderingChildService) AcknowledgeHandoff(ctx context.Context) error {
	select {
	case <-service.observed:
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(5 * time.Second):
		return errors.New("HTTP acceptance was not observed after flush")
	}
	if err := service.provisioner.Destroy(ctx, &provision.Resources{SessionID: service.bootstrap.SessionID, Instance: "session-" + service.bootstrap.SessionID, Volume: "workspace-" + service.bootstrap.SessionID}); err != nil {
		return err
	}
	close(service.events)
	return nil
}
func (*orderingChildService) Stop(context.Context, string) error { return nil }

func runOrderingChildHelper(t *testing.T) {
	bootstrapFile := os.NewFile(3, "bootstrap")
	livenessFile := os.NewFile(4, "liveness")
	reportFile := os.NewFile(5, "report")
	ackFile := os.NewFile(6, "terminal-ack")
	if bootstrapFile == nil || livenessFile == nil || reportFile == nil || ackFile == nil {
		t.Fatal("ordering helper inherited descriptors are missing")
	}
	bootstrap, err := process.DecodeBootstrap(bootstrapFile)
	if err != nil {
		t.Fatal(err)
	}
	_ = bootstrapFile.Close()
	listener, err := net.Listen("unix", bootstrap.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(bootstrap.SocketPath, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(bootstrap.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	reporter := process.NewAcknowledgedReporter(context.Background(), reportFile, ackFile, bootstrap.SessionID)
	ticket := provision.RecoveryTicket{SessionID: bootstrap.SessionID, ParentID: bootstrap.ParentID, RootID: bootstrap.RootID, Pool: "pool", Instance: "session-" + bootstrap.SessionID, Volume: "workspace-" + bootstrap.SessionID, SocketPath: bootstrap.SocketPath, Socket: provision.SocketIdentity{Device: uint64(stat.Dev), Inode: stat.Ino}, Kind: bootstrap.Request.Kind, Context: bootstrap.Request.Context, WorkerType: bootstrap.Request.WorkerType, RunAttribution: bootstrap.RunAttribution}
	if err := reporter.Ownership(ticket); err != nil {
		t.Fatal(err)
	}
	orderPath := os.Getenv("KANEDIAS_ORDERING_FILE")
	service := &orderingChildService{bootstrap: bootstrap, reporter: reporter, provisioner: orderingFakeChildProvisioner{orderPath: orderPath}, orderPath: orderPath, events: make(chan supervisor.EventEnvelope), observed: make(chan struct{}), ready: make(chan struct{})}
	server := &http.Server{Handler: supervisorapi.NewHandler(service)}
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Serve(listener) }()
	if err := reporter.Ready(bootstrap.SocketPath); err != nil {
		t.Fatal(err)
	}
	select {
	case <-service.ready:
	case <-time.After(5 * time.Second):
		t.Fatal("parent never probed child tree")
	}

	transport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", bootstrap.SocketPath)
	}}
	client := &http.Client{Transport: transport}
	requestBody, _ := json.Marshal(supervisor.WriteHandoffRequest{Repositories: []contract.RepositoryHandoff{{Repository: "owner/disposable", BaseCommit: strings.Repeat("b", 40), Branch: "feature", HeadCommit: strings.Repeat("a", 40)}}, Summary: "done", Verification: []string{"go test ./..."}})
	request, _ := http.NewRequest(http.MethodPost, "http://unix/v1/handoff", bytes.NewReader(requestBody))
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	// Do cannot return until the production handler has written and flushed the
	// acceptance headers. Release acknowledgement cleanup only after observing
	// that real HTTP boundary, then verify the complete accepted body.
	if response.StatusCode != http.StatusOK {
		t.Fatalf("handoff acceptance status = HTTP %d", response.StatusCode)
	}
	if err := appendOrderingStep(service.orderPath, "acceptance"); err != nil {
		t.Fatal(err)
	}
	close(service.observed)
	body, readErr := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if readErr != nil || !bytes.Contains(body, []byte(`"accepted":true`)) {
		t.Fatalf("handoff acceptance = HTTP %d %s, %v", response.StatusCode, body, readErr)
	}
	transport.CloseIdleConnections()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		t.Fatal(err)
	}
	if err := <-serverDone; err != nil && !errors.Is(err, http.ErrServerClosed) {
		t.Fatal(err)
	}
	_ = listener.Close()
	_ = os.Remove(bootstrap.SocketPath)
	_ = livenessFile.Close()
	_ = reportFile.Close()
	_ = ackFile.Close()
	if err := appendOrderingStep(service.orderPath, "exit"); err != nil {
		t.Fatal(err)
	}
}

func runCleanEOFChildHelper(t *testing.T) {
	bootstrapFile := os.NewFile(3, "bootstrap")
	livenessFile := os.NewFile(4, "liveness")
	reportFile := os.NewFile(5, "report")
	ackFile := os.NewFile(6, "terminal-ack")
	bootstrap, err := process.DecodeBootstrap(bootstrapFile)
	if err != nil {
		t.Fatal(err)
	}
	_ = bootstrapFile.Close()
	listener, err := net.Listen("unix", bootstrap.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(bootstrap.SocketPath, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(bootstrap.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	reporter := process.NewAcknowledgedReporter(context.Background(), reportFile, ackFile, bootstrap.SessionID)
	ticket := provision.RecoveryTicket{SessionID: bootstrap.SessionID, ParentID: bootstrap.ParentID, RootID: bootstrap.RootID, Pool: "pool", Instance: "session-" + bootstrap.SessionID, Volume: "workspace-" + bootstrap.SessionID, SocketPath: bootstrap.SocketPath, Socket: provision.SocketIdentity{Device: uint64(stat.Dev), Inode: stat.Ino}, Kind: bootstrap.Request.Kind, Context: bootstrap.Request.Context, WorkerType: bootstrap.Request.WorkerType}
	if err := reporter.Ownership(ticket); err != nil {
		t.Fatal(err)
	}
	closedEvents := make(chan supervisor.EventEnvelope)
	close(closedEvents)
	orderPath := os.Getenv("KANEDIAS_ORDERING_FILE")
	service := &orderingChildService{bootstrap: bootstrap, reporter: reporter, provisioner: orderingFakeChildProvisioner{orderPath: orderPath}, orderPath: orderPath, events: closedEvents, observed: make(chan struct{}), ready: make(chan struct{})}
	server := &http.Server{Handler: supervisorapi.NewHandler(service)}
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Serve(listener) }()
	if err := reporter.Ready(bootstrap.SocketPath); err != nil {
		t.Fatal(err)
	}
	select {
	case <-service.ready:
	case <-time.After(5 * time.Second):
		t.Fatal("parent never probed child tree")
	}
	if _, err := io.Copy(io.Discard, livenessFile); err != nil {
		t.Fatal(err)
	}
	if err := appendOrderingStep(service.orderPath, "liveness-closed"); err != nil {
		t.Fatal(err)
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		t.Fatal(err)
	}
	if err := <-serverDone; err != nil && !errors.Is(err, http.ErrServerClosed) {
		t.Fatal(err)
	}
	_ = listener.Close()
	_ = os.Remove(bootstrap.SocketPath)
	_ = livenessFile.Close()
	_ = reportFile.Close()
	_ = ackFile.Close()
}

func appendOrderingStep(path, step string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := fmt.Fprintln(file, step)
	return errors.Join(writeErr, file.Close())
}

func readOrderingSteps(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return strings.Fields(string(data))
}
