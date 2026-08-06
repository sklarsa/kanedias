package session

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lxc/incus/v7/shared/api"
	"github.com/sklarsa/kanedias/internal/config"
	"github.com/sklarsa/kanedias/internal/proxy"
)

type cleanupContextObservation struct {
	err      error
	deadline time.Time
	hasLimit bool
}

type recordingSessionClient struct {
	calls          *[]string
	createdRequest api.InstancesPost
	copyErr        error
	createErr      error
	startErr       error
	stateErr       error
	stateStatus    api.StatusCode
	stateContexts  []cleanupContextObservation
	stopForces     []bool
	cleanup        []cleanupContextObservation
}

func observeCleanupContext(ctx context.Context) cleanupContextObservation {
	deadline, hasLimit := ctx.Deadline()
	return cleanupContextObservation{err: ctx.Err(), deadline: deadline, hasLimit: hasLimit}
}

func (c *recordingSessionClient) record(call string) { *c.calls = append(*c.calls, call) }
func (c *recordingSessionClient) Disconnect()        {}
func (c *recordingSessionClient) ResolvePool(context.Context, string) (string, error) {
	c.record("resolve-pool")
	return "pool1", nil
}
func (c *recordingSessionClient) GetNetwork(context.Context, string) (*api.Network, error) {
	return nil, errors.New("unexpected GetNetwork call")
}
func (c *recordingSessionClient) CreateNetwork(context.Context, api.NetworksPost) error {
	return errors.New("unexpected CreateNetwork call")
}
func (c *recordingSessionClient) EnsureProfile(_ context.Context, name string, _ []byte) error {
	c.record("ensure-profile " + name)
	return nil
}
func (c *recordingSessionClient) GetImageAlias(_ context.Context, name string) (*api.ImageAliasesEntry, error) {
	c.record("get-image " + name)
	return &api.ImageAliasesEntry{Name: name}, nil
}
func (c *recordingSessionClient) GetStorageVolume(_ context.Context, _, name string) (*api.StorageVolume, error) {
	c.record("get-volume " + name)
	return &api.StorageVolume{Name: name}, nil
}
func (c *recordingSessionClient) CopyStorageVolume(_ context.Context, _, source, target string) error {
	c.record("copy-volume " + source + " " + target)
	return c.copyErr
}
func (c *recordingSessionClient) DeleteStorageVolume(ctx context.Context, _, name string) error {
	c.cleanup = append(c.cleanup, observeCleanupContext(ctx))
	c.record("delete-volume " + name)
	return nil
}
func (c *recordingSessionClient) CreateInstance(_ context.Context, request api.InstancesPost) error {
	c.createdRequest = request
	c.record("create-instance")
	return c.createErr
}
func (c *recordingSessionClient) StartInstance(context.Context, string) error {
	c.record("start-instance")
	return c.startErr
}
func (c *recordingSessionClient) StopInstance(ctx context.Context, _ string, force bool) error {
	c.cleanup = append(c.cleanup, observeCleanupContext(ctx))
	c.stopForces = append(c.stopForces, force)
	c.record("stop-instance")
	return nil
}
func (c *recordingSessionClient) DeleteInstance(ctx context.Context, _ string) error {
	c.cleanup = append(c.cleanup, observeCleanupContext(ctx))
	c.record("delete-instance")
	return nil
}
func (c *recordingSessionClient) GetInstanceState(ctx context.Context, _ string) (*api.InstanceState, error) {
	c.stateContexts = append(c.stateContexts, observeCleanupContext(ctx))
	c.record("get-instance-state")
	if c.stateErr != nil {
		return nil, c.stateErr
	}
	status := c.stateStatus
	if status == 0 {
		status = api.Running
	}
	return &api.InstanceState{
		StatusCode: status,
		Network: map[string]api.InstanceStateNetwork{
			"eth0": {Addresses: []api.InstanceStateNetworkAddress{
				{Family: "inet", Scope: "global", Address: "10.76.111.42"},
			}},
		},
	}, nil
}

func testDependencies(client sessionClient, calls *[]string, peerDone chan<- struct{}) dependencies {
	return dependencies{
		connect: func(context.Context) (sessionClient, error) { return client, nil },
		ensureNetwork: func(context.Context, sessionClient, config.Config) error {
			*calls = append(*calls, "ensure-network")
			return nil
		},
		renderProfile: func(w io.Writer, name string, _ config.Config) error {
			_, err := io.WriteString(w, "name: "+name)
			return err
		},
		defaultProxyOpts: func() (proxy.Options, error) {
			return proxy.Options{CACertPath: "ca.crt", CAKeyPath: "ca.key"}, nil
		},
		initCA: func(_, _ string) error {
			*calls = append(*calls, "init-ca")
			return nil
		},
		checkProxy: func(context.Context, config.Config) error {
			*calls = append(*calls, "check-proxy")
			return nil
		},
		dialRPC: func(_ context.Context, address string) (net.Conn, error) {
			*calls = append(*calls, "dial "+address)
			clientConn, serverConn := net.Pipe()
			go func() {
				defer close(peerDone)
				defer serverConn.Close()
				var command promptCommand
				if err := json.NewDecoder(serverConn).Decode(&command); err != nil {
					return
				}
				if command.Message != "keep this prompt\n" {
					return
				}
				_, _ = io.WriteString(serverConn,
					"{\"id\":\"prompt-1\",\"type\":\"response\",\"command\":\"prompt\",\"success\":true}\n"+
						"{\"type\":\"agent_settled\"}\n")
			}()
			return clientConn, nil
		},
		operationWasSubmitted: func(err error) bool {
			var submitted submittedTestError
			return errors.As(err, &submitted)
		},
		newName:          func() (string, error) { return "session-test", nil },
		readinessTimeout: time.Second,
		retryInterval:    time.Millisecond,
	}
}

func validSessionConfig() config.Config {
	return config.Config{
		Network:   config.Network{IPv4: "10.76.111.1/24"},
		BaseImage: config.BaseImage{Name: "sandbox", Source: "images:", Image: "debian/12"},
		Workspace: config.Workspace{Volume: config.DefaultWorkspaceVolume},
	}
}

func TestRunHappyPath(t *testing.T) {
	var calls []string
	client := &recordingSessionClient{calls: &calls}
	peerDone := make(chan struct{})
	deps := testDependencies(client, &calls, peerDone)
	var stdout, stderr bytes.Buffer

	if err := run(context.Background(), validSessionConfig(), "keep this prompt\n", &stdout, &stderr, deps); err != nil {
		t.Fatalf("run: %v", err)
	}
	<-peerDone

	wantCalls := []string{
		"resolve-pool",
		"init-ca",
		"ensure-network",
		"ensure-profile sandbox",
		"check-proxy",
		"get-image sandbox",
		"get-volume kanedias-workspace-seed",
		"copy-volume kanedias-workspace-seed kanedias-workspace-session-test",
		"create-instance",
		"start-instance",
		"get-instance-state",
		"dial 10.76.111.42:7777",
		"stop-instance",
		"delete-instance",
		"delete-volume kanedias-workspace-session-test",
	}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("calls = %#v, want %#v", calls, wantCalls)
	}

	request := client.createdRequest
	if request.Name != "session-test" || !reflect.DeepEqual(request.Profiles, []string{"default", "sandbox"}) {
		t.Errorf("instance identity/profiles = %#v", request)
	}
	if !reflect.DeepEqual(request.Source, api.InstanceSource{Type: "image", Alias: "sandbox"}) {
		t.Errorf("source = %#v", request.Source)
	}
	wantConfig := api.ConfigMap{
		"user.kanedias.kind":     "session",
		"user.kanedias.rpc.port": "7777",
	}
	if !reflect.DeepEqual(request.Config, wantConfig) {
		t.Errorf("config = %#v, want %#v", request.Config, wantConfig)
	}
	wantDevice := map[string]string{
		"type": "disk", "pool": "pool1", "source": "kanedias-workspace-session-test", "path": "/workspace",
	}
	if !reflect.DeepEqual(request.Devices["workspace"], wantDevice) {
		t.Errorf("workspace device = %#v, want %#v", request.Devices["workspace"], wantDevice)
	}
	wantRoot := map[string]string{"type": "disk", "pool": "pool1", "path": "/"}
	if !reflect.DeepEqual(request.Devices["root"], wantRoot) {
		t.Errorf("root device = %#v, want %#v", request.Devices["root"], wantRoot)
	}

	const records = "{\"id\":\"prompt-1\",\"type\":\"response\",\"command\":\"prompt\",\"success\":true}\n" +
		"{\"type\":\"agent_settled\"}\n"
	if stdout.String() != records {
		t.Errorf("stdout = %q, want only RPC records %q", stdout.String(), records)
	}
	for _, progress := range []string{"Creating session session-test...", "Waiting for Pi RPC in session-test...", "Stopping session session-test...", "Deleting session session-test..."} {
		if !strings.Contains(stderr.String(), progress) {
			t.Errorf("stderr %q does not contain %q", stderr.String(), progress)
		}
	}
}

type submittedTestError struct{ err error }

func (e submittedTestError) Error() string { return e.err.Error() }
func (e submittedTestError) Unwrap() error { return e.err }

func TestRunOwnsResourcesAfterSubmittedOperationWaitFailures(t *testing.T) {
	waitErr := errors.New("operation wait failed")
	tests := []struct {
		name      string
		configure func(*recordingSessionClient)
		wantTail  []string
	}{
		{
			name: "copy",
			configure: func(client *recordingSessionClient) {
				client.copyErr = submittedTestError{err: waitErr}
			},
			wantTail: []string{
				"copy-volume kanedias-workspace-seed kanedias-workspace-session-test",
				"delete-volume kanedias-workspace-session-test",
			},
		},
		{
			name: "create",
			configure: func(client *recordingSessionClient) {
				client.createErr = submittedTestError{err: waitErr}
			},
			wantTail: []string{
				"create-instance",
				"delete-instance",
				"delete-volume kanedias-workspace-session-test",
			},
		},
		{
			name: "start",
			configure: func(client *recordingSessionClient) {
				client.startErr = submittedTestError{err: waitErr}
				client.stateStatus = api.Running
			},
			wantTail: []string{
				"start-instance",
				"get-instance-state",
				"stop-instance",
				"delete-instance",
				"delete-volume kanedias-workspace-session-test",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var calls []string
			client := &recordingSessionClient{calls: &calls}
			test.configure(client)
			deps := testDependencies(client, &calls, make(chan struct{}))
			deps.dialRPC = func(context.Context, string) (net.Conn, error) {
				t.Fatal("dialRPC called after operation failure")
				return nil, nil
			}

			err := run(context.Background(), validSessionConfig(), "prompt", io.Discard, io.Discard, deps)
			if !errors.Is(err, waitErr) {
				t.Fatalf("run error = %v, want wait failure", err)
			}
			if got := calls[len(calls)-len(test.wantTail):]; !reflect.DeepEqual(got, test.wantTail) {
				t.Fatalf("calls tail = %#v, want %#v", got, test.wantTail)
			}
			if test.name == "start" {
				if !reflect.DeepEqual(client.stopForces, []bool{true}) {
					t.Fatalf("stop force values = %#v, want [true]", client.stopForces)
				}
				if len(client.stateContexts) != 1 {
					t.Fatalf("state query contexts = %d, want 1", len(client.stateContexts))
				}
				observation := client.stateContexts[0]
				if observation.err != nil || !observation.hasLimit {
					t.Fatalf("state query cleanup context = %#v, want uncancelled and bounded", observation)
				}
				remaining := time.Until(observation.deadline)
				if remaining <= 0 || remaining > cleanupTimeout {
					t.Fatalf("state query deadline remaining = %v, want within %v", remaining, cleanupTimeout)
				}
			}
		})
	}
}

func TestRunDoesNotOwnResourcesAfterImmediateSubmissionFailures(t *testing.T) {
	submitErr := errors.New("submission rejected")
	tests := []struct {
		name      string
		configure func(*recordingSessionClient)
		forbidden []string
	}{
		{
			name: "copy",
			configure: func(client *recordingSessionClient) {
				client.copyErr = submitErr
			},
			forbidden: []string{"delete-volume", "delete-instance", "stop-instance"},
		},
		{
			name: "create",
			configure: func(client *recordingSessionClient) {
				client.createErr = submitErr
			},
			forbidden: []string{"delete-instance", "stop-instance"},
		},
		{
			name: "start",
			configure: func(client *recordingSessionClient) {
				client.startErr = submitErr
			},
			forbidden: []string{"stop-instance", "get-instance-state"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var calls []string
			client := &recordingSessionClient{calls: &calls}
			test.configure(client)
			deps := testDependencies(client, &calls, make(chan struct{}))
			err := run(context.Background(), validSessionConfig(), "prompt", io.Discard, io.Discard, deps)
			if !errors.Is(err, submitErr) {
				t.Fatalf("run error = %v, want submission failure", err)
			}
			joined := strings.Join(calls, "\n")
			for _, forbidden := range test.forbidden {
				if strings.Contains(joined, forbidden) {
					t.Fatalf("calls include unowned cleanup %q: %#v", forbidden, calls)
				}
			}
		})
	}
}

func TestRunCancellationClosesRPCAndCleansUpOwnedResources(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls []string
	client := &recordingSessionClient{calls: &calls}
	deps := testDependencies(client, &calls, make(chan struct{}))
	promptReceived := make(chan struct{})
	peerClosed := make(chan struct{})
	deps.dialRPC = func(_ context.Context, address string) (net.Conn, error) {
		calls = append(calls, "dial "+address)
		clientConn, serverConn := net.Pipe()
		go func() {
			defer close(peerClosed)
			defer serverConn.Close()
			reader := bufio.NewReader(serverConn)
			if _, err := reader.ReadBytes('\n'); err != nil {
				return
			}
			close(promptReceived)
			_, _ = reader.ReadByte()
		}()
		return clientConn, nil
	}

	result := make(chan error, 1)
	go func() {
		result <- run(ctx, validSessionConfig(), "prompt", io.Discard, io.Discard, deps)
	}()
	<-promptReceived
	cancel()

	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("run error = %v, want context cancellation", err)
	}
	select {
	case <-peerClosed:
	case <-time.After(time.Second):
		t.Fatal("RPC peer did not observe the socket closing after cancellation")
	}
	wantCleanup := []string{"stop-instance", "delete-instance", "delete-volume kanedias-workspace-session-test"}
	if got := calls[len(calls)-len(wantCleanup):]; !reflect.DeepEqual(got, wantCleanup) {
		t.Fatalf("cleanup calls = %#v, want %#v", got, wantCleanup)
	}
}

func TestRunCleansUpAfterStartFailureWithUncancelledBoundedContext(t *testing.T) {
	startErr := errors.New("start failed")
	var calls []string
	client := &recordingSessionClient{calls: &calls, startErr: startErr}
	deps := testDependencies(client, &calls, make(chan struct{}))
	deps.dialRPC = func(context.Context, string) (net.Conn, error) {
		t.Fatal("dialRPC called after start failure")
		return nil, nil
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout, stderr bytes.Buffer
	err := run(cancelled, validSessionConfig(), "prompt", &stdout, &stderr, deps)
	if !errors.Is(err, startErr) {
		t.Fatalf("run error = %v, want %v", err, startErr)
	}
	if got := calls[len(calls)-2:]; !reflect.DeepEqual(got, []string{"delete-instance", "delete-volume kanedias-workspace-session-test"}) {
		t.Errorf("cleanup calls = %#v", got)
	}
	if strings.Contains(strings.Join(calls, "\n"), "dial ") {
		t.Errorf("calls unexpectedly include RPC dial: %#v", calls)
	}
	if len(client.cleanup) != 2 {
		t.Fatalf("cleanup contexts = %d, want 2", len(client.cleanup))
	}
	for _, observation := range client.cleanup {
		if observation.err != nil {
			t.Errorf("cleanup context is cancelled: %v", observation.err)
		}
		if !observation.hasLimit {
			t.Error("cleanup context has no deadline")
			continue
		}
		remaining := time.Until(observation.deadline)
		if remaining <= 0 || remaining > 30*time.Second {
			t.Errorf("cleanup deadline remaining = %v, want within 30s", remaining)
		}
	}
}
