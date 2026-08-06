package session

import (
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
	startErr       error
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
	return nil
}
func (c *recordingSessionClient) DeleteStorageVolume(ctx context.Context, _, name string) error {
	c.cleanup = append(c.cleanup, observeCleanupContext(ctx))
	c.record("delete-volume " + name)
	return nil
}
func (c *recordingSessionClient) CreateInstance(_ context.Context, request api.InstancesPost) error {
	c.createdRequest = request
	c.record("create-instance")
	return nil
}
func (c *recordingSessionClient) StartInstance(context.Context, string) error {
	c.record("start-instance")
	return c.startErr
}
func (c *recordingSessionClient) StopInstance(ctx context.Context, _ string, _ bool) error {
	c.cleanup = append(c.cleanup, observeCleanupContext(ctx))
	c.record("stop-instance")
	return nil
}
func (c *recordingSessionClient) DeleteInstance(ctx context.Context, _ string) error {
	c.cleanup = append(c.cleanup, observeCleanupContext(ctx))
	c.record("delete-instance")
	return nil
}
func (c *recordingSessionClient) GetInstanceState(context.Context, string) (*api.InstanceState, error) {
	c.record("get-instance-state")
	return &api.InstanceState{Network: map[string]api.InstanceStateNetwork{
		"eth0": {Addresses: []api.InstanceStateNetworkAddress{
			{Family: "inet", Scope: "global", Address: "10.76.111.42"},
		}},
	}}, nil
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
