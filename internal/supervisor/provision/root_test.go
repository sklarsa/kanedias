package provision

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/lxc/incus/v7/shared/api"
	"github.com/sklarsa/kanedias/internal/config"
)

type recordingRootClient struct {
	calls         []string
	copyErr       error
	createErr     error
	startErr      error
	stopErr       error
	deleteInstErr error
	deleteVolErr  error
	request       api.InstancesPost
	volumePut     api.StorageVolumePut
	lateVolume    bool
	volumeReady   bool
	lateInstance  bool
	instanceReady bool
}

func (client *recordingRootClient) Disconnect() { client.calls = append(client.calls, "disconnect") }
func (client *recordingRootClient) ResolvePool(context.Context, string) (string, error) {
	client.calls = append(client.calls, "resolve-pool")
	return "btrfs", nil
}
func (client *recordingRootClient) GetNetwork(context.Context, string) (*api.Network, error) {
	client.calls = append(client.calls, "get-network")
	return &api.Network{Name: "kanedias", Type: "bridge", Managed: true, NetworkPut: api.NetworkPut{Config: api.ConfigMap{"ipv4.address": "10.55.0.1/24", "ipv4.nat": "true"}}}, nil
}
func (client *recordingRootClient) CreateNetwork(context.Context, api.NetworksPost) error { return nil }
func (client *recordingRootClient) EnsureProfile(context.Context, string, []byte) error {
	client.calls = append(client.calls, "ensure-profile")
	return nil
}
func (client *recordingRootClient) GetImageAlias(context.Context, string) (*api.ImageAliasesEntry, error) {
	client.calls = append(client.calls, "get-image")
	return &api.ImageAliasesEntry{Name: "base"}, nil
}
func (client *recordingRootClient) GetStorageVolume(_ context.Context, _ string, name string) (*api.StorageVolume, error) {
	if name == "seed" {
		client.calls = append(client.calls, "get-seed")
		return &api.StorageVolume{Name: "seed"}, nil
	}
	client.calls = append(client.calls, "probe-volume")
	if client.lateVolume && !client.volumeReady {
		return nil, api.StatusErrorf(http.StatusNotFound, "not materialized")
	}
	return &api.StorageVolume{Name: name}, nil
}
func (client *recordingRootClient) CopyStorageVolume(context.Context, string, string, string) error {
	client.calls = append(client.calls, "copy-volume")
	return client.copyErr
}
func (client *recordingRootClient) GetStorageVolumeWithETag(_ context.Context, _ string, name string) (*api.StorageVolume, string, error) {
	client.calls = append(client.calls, "get-owned-volume")
	return &api.StorageVolume{Name: name, StorageVolumePut: api.StorageVolumePut{Config: api.ConfigMap{"source": "seed"}}}, "etag", nil
}
func (client *recordingRootClient) UpdateStorageVolume(_ context.Context, _, _ string, request api.StorageVolumePut, etag string) error {
	client.calls = append(client.calls, "tag-volume")
	if etag != "etag" {
		return errors.New("unexpected volume ETag")
	}
	client.volumePut = request
	return nil
}
func (client *recordingRootClient) DeleteStorageVolume(context.Context, string, string) error {
	client.calls = append(client.calls, "delete-volume")
	return client.deleteVolErr
}
func (client *recordingRootClient) CreateInstance(_ context.Context, request api.InstancesPost) error {
	client.calls = append(client.calls, "create-instance")
	client.request = request
	return client.createErr
}
func (client *recordingRootClient) StartInstance(context.Context, string) error {
	client.calls = append(client.calls, "start-instance")
	return client.startErr
}
func (client *recordingRootClient) StopInstance(context.Context, string, bool) error {
	client.calls = append(client.calls, "stop-instance")
	return client.stopErr
}
func (client *recordingRootClient) DeleteInstance(context.Context, string) error {
	client.calls = append(client.calls, "delete-instance")
	return client.deleteInstErr
}
func (client *recordingRootClient) GetInstanceState(context.Context, string) (*api.InstanceState, error) {
	client.calls = append(client.calls, "get-state")
	if client.lateInstance && !client.instanceReady {
		return nil, api.StatusErrorf(http.StatusNotFound, "not materialized")
	}
	return &api.InstanceState{StatusCode: api.Running, Network: map[string]api.InstanceStateNetwork{"eth0": {Addresses: []api.InstanceStateNetworkAddress{{Family: "inet", Scope: "global", Address: "10.55.0.9"}}}}}, nil
}

func rootTestConfig() config.Config {
	return config.Config{
		Network:   config.Network{IPv4: "10.55.0.1/24"},
		BaseImage: config.BaseImage{Name: "base", Source: "images:", Image: "ubuntu"},
		Workspace: config.Workspace{Pool: "btrfs", Volume: "seed"},
	}
}

func validRootRequest() RootRequest {
	return RootRequest{
		SessionID:  "root-1",
		SocketPath: "/tmp/root.sock",
		Model: config.ModelProfile{
			Provider: "openai-codex", Model: "gpt-5.6-sol", ThinkingLevel: "high",
		},
	}
}

func testRootDependencies(client *recordingRootClient) rootDependencies {
	return rootDependencies{
		connect: func(context.Context) (rootClient, error) { return client, nil },
		ensureNetwork: func(ctx context.Context, got rootClient, cfg config.Config) error {
			_, err := got.GetNetwork(ctx, "kanedias")
			return err
		},
		renderProfile: func(io.Writer, string, config.Config) error { return nil },
		initProxyCA:   func() error { return nil },
		checkProxy: func(context.Context, config.Config) error {
			client.calls = append(client.calls, "check-proxy")
			return nil
		},
		operationWasSubmitted:   func(error) bool { return false },
		awaitSubmittedOperation: func(context.Context, error) error { return nil },
		newName:                 func() (string, error) { return "session-test", nil },
		readinessTimeout:        time.Second,
		retryInterval:           time.Millisecond,
	}
}

func TestRootProvisionerChecksProxyBeforeOwnedResources(t *testing.T) {
	client := &recordingRootClient{}
	deps := testRootDependencies(client)
	proxyErr := errors.New("proxy down")
	deps.checkProxy = func(context.Context, config.Config) error {
		client.calls = append(client.calls, "check-proxy")
		return proxyErr
	}
	provisioner := newRootProvisioner(rootTestConfig(), deps)

	resources, err := provisioner.ProvisionRoot(context.Background(), validRootRequest())
	if !errors.Is(err, proxyErr) || resources != nil {
		t.Fatalf("ProvisionRoot() = (%v, %v), want nil resources and proxy error", resources, err)
	}
	calls := strings.Join(client.calls, ",")
	if strings.Contains(calls, "copy-volume") || strings.Contains(calls, "create-instance") {
		t.Fatalf("owned resource created before proxy preflight: %s", calls)
	}
}

func TestRootProvisionerCreatesSocketProxyBeforeStarting(t *testing.T) {
	client := &recordingRootClient{}
	provisioner := newRootProvisioner(rootTestConfig(), testRootDependencies(client))

	resources, err := provisioner.ProvisionRoot(context.Background(), validRootRequest())
	if err != nil {
		t.Fatalf("ProvisionRoot() error = %v", err)
	}
	if resources.Pool != "btrfs" || resources.RPCAddr != "10.55.0.9:7777" {
		t.Fatalf("resources = %#v", resources)
	}
	device := client.request.Devices["supervisor"]
	want := map[string]string{"type": "proxy", "bind": "instance", "listen": "unix:/run/kanedias/supervisor.sock", "connect": "unix:/tmp/root.sock", "uid": "1000", "gid": "1000", "mode": "0600"}
	for key, value := range want {
		if device[key] != value {
			t.Errorf("supervisor device[%q] = %q, want %q", key, device[key], value)
		}
	}
	wantEnvironment := map[string]string{
		"environment.KANEDIAS_SESSION_ID": "root-1", "environment.KANEDIAS_SESSION_KIND": "root",
		"environment.KANEDIAS_WORKER_TYPE": "", "environment.KANEDIAS_PI_PROVIDER": "openai-codex",
		"environment.KANEDIAS_PI_MODEL": "gpt-5.6-sol", "environment.KANEDIAS_PI_THINKING": "high",
		"environment.KANEDIAS_PI_SESSION_FILE": "", "environment.KANEDIAS_SUPERVISOR_SOCKET": "/run/kanedias/supervisor.sock",
	}
	for key, value := range wantEnvironment {
		if client.request.Config[key] != value {
			t.Errorf("root environment %q = %q, want %q", key, client.request.Config[key], value)
		}
	}
	if client.request.Devices["root"]["pool"] != resources.Pool || client.request.Devices["workspace"]["pool"] != resources.Pool {
		t.Fatalf("root/workspace devices do not use effective pool %q: %#v", resources.Pool, client.request.Devices)
	}
	if got := client.volumePut.Config["user.kanedias.session_id"]; got != "root-1" {
		t.Fatalf("root volume session metadata = %q, want root-1", got)
	}
	if got := client.volumePut.Config["user.kanedias.kind"]; got != "root" {
		t.Fatalf("root volume kind metadata = %q, want root", got)
	}
	if got := client.volumePut.Config["user.kanedias.workspace_volume"]; got != resources.Volume {
		t.Fatalf("root volume name metadata = %q, want %q", got, resources.Volume)
	}
	calls := strings.Join(client.calls, ",")
	if strings.Index(calls, "create-instance") > strings.Index(calls, "start-instance") || device == nil {
		t.Fatalf("instance started before proxy device was submitted: %s", calls)
	}
}

func TestRootProvisionerRejectsInvalidModelBeforeConnecting(t *testing.T) {
	for _, test := range []struct {
		name  string
		model config.ModelProfile
	}{
		{name: "missing provider", model: config.ModelProfile{Model: "gpt-5.6-sol", ThinkingLevel: "high"}},
		{name: "missing model", model: config.ModelProfile{Provider: "openai-codex", ThinkingLevel: "high"}},
		{name: "empty thinking", model: config.ModelProfile{Provider: "openai-codex", Model: "gpt-5.6-sol"}},
		{name: "invalid thinking", model: config.ModelProfile{Provider: "openai-codex", Model: "gpt-5.6-sol", ThinkingLevel: "extreme"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &recordingRootClient{}
			provisioner := newRootProvisioner(rootTestConfig(), testRootDependencies(client))
			request := validRootRequest()
			request.Model = test.model
			if resources, err := provisioner.ProvisionRoot(context.Background(), request); resources != nil || err == nil {
				t.Fatalf("ProvisionRoot() = (%v, %v), want validation failure", resources, err)
			}
			if len(client.calls) != 0 {
				t.Fatalf("Incus calls = %v, want none", client.calls)
			}
		})
	}
}

func TestRootProvisionerAwaitsSubmittedVolumeBeforeProbingLateMaterialization(t *testing.T) {
	primary := errors.New("copy wait cancelled")
	client := &recordingRootClient{copyErr: primary, lateVolume: true}
	deps := testRootDependencies(client)
	deps.operationWasSubmitted = func(err error) bool { return errors.Is(err, primary) }
	deps.awaitSubmittedOperation = func(_ context.Context, err error) error {
		if !errors.Is(err, primary) {
			t.Fatalf("await error = %v", err)
		}
		client.calls = append(client.calls, "await-operation")
		if _, probeErr := client.GetStorageVolume(context.Background(), "btrfs", rootWorkspaceNamePrefix+"session-test"); !api.StatusErrorCheck(probeErr, http.StatusNotFound) {
			t.Fatalf("first pre-terminal probe = %v, want 404", probeErr)
		}
		client.volumeReady = true
		return nil
	}
	provisioner := newRootProvisioner(rootTestConfig(), deps)
	if resources, err := provisioner.ProvisionRoot(context.Background(), validRootRequest()); resources != nil || !errors.Is(err, primary) {
		t.Fatalf("ProvisionRoot() = (%v, %v)", resources, err)
	}
	calls := strings.Join(client.calls, ",")
	if !strings.Contains(calls, "await-operation,probe-volume,probe-volume,delete-volume") {
		t.Fatalf("late volume cleanup order = %s", calls)
	}
}

func TestRootProvisionerAwaitsSubmittedCreateBeforeProbingLateInstance(t *testing.T) {
	primary := errors.New("create wait cancelled")
	client := &recordingRootClient{createErr: primary, lateInstance: true}
	deps := testRootDependencies(client)
	deps.operationWasSubmitted = func(err error) bool { return errors.Is(err, primary) }
	deps.awaitSubmittedOperation = func(_ context.Context, err error) error {
		if !errors.Is(err, primary) {
			t.Fatalf("await error = %v", err)
		}
		client.calls = append(client.calls, "await-operation")
		if _, probeErr := client.GetInstanceState(context.Background(), "session-test"); !api.StatusErrorCheck(probeErr, http.StatusNotFound) {
			t.Fatalf("first pre-terminal probe = %v, want 404", probeErr)
		}
		client.instanceReady = true
		return nil
	}
	provisioner := newRootProvisioner(rootTestConfig(), deps)
	if resources, err := provisioner.ProvisionRoot(context.Background(), validRootRequest()); resources != nil || !errors.Is(err, primary) {
		t.Fatalf("ProvisionRoot() = (%v, %v)", resources, err)
	}
	calls := strings.Join(client.calls, ",")
	if !strings.Contains(calls, "await-operation,get-state,get-state,stop-instance,delete-instance,delete-volume") {
		t.Fatalf("late instance cleanup order = %s", calls)
	}
}

func TestRootProvisionerAwaitsSubmittedStartBeforeCleanupProbe(t *testing.T) {
	primary := errors.New("start wait cancelled")
	client := &recordingRootClient{startErr: primary, lateInstance: true}
	deps := testRootDependencies(client)
	deps.operationWasSubmitted = func(err error) bool { return errors.Is(err, primary) }
	deps.awaitSubmittedOperation = func(_ context.Context, err error) error {
		if !errors.Is(err, primary) {
			t.Fatalf("await error = %v", err)
		}
		client.calls = append(client.calls, "await-operation")
		if _, probeErr := client.GetInstanceState(context.Background(), "session-test"); !api.StatusErrorCheck(probeErr, http.StatusNotFound) {
			t.Fatalf("first pre-terminal probe = %v, want 404", probeErr)
		}
		client.instanceReady = true
		return nil
	}
	provisioner := newRootProvisioner(rootTestConfig(), deps)
	if resources, err := provisioner.ProvisionRoot(context.Background(), validRootRequest()); resources != nil || !errors.Is(err, primary) {
		t.Fatalf("ProvisionRoot() = (%v, %v)", resources, err)
	}
	calls := strings.Join(client.calls, ",")
	if !strings.Contains(calls, "start-instance,await-operation,get-state,get-state,stop-instance,delete-instance,delete-volume") {
		t.Fatalf("late start cleanup order = %s", calls)
	}
}

func TestRootProvisionerCleansSubmittedAmbiguityAndJoinsCleanupError(t *testing.T) {
	primary := errors.New("create wait failed")
	cleanup := errors.New("delete instance failed")
	client := &recordingRootClient{createErr: primary, deleteInstErr: cleanup}
	deps := testRootDependencies(client)
	deps.operationWasSubmitted = func(err error) bool { return errors.Is(err, primary) }
	provisioner := newRootProvisioner(rootTestConfig(), deps)

	resources, err := provisioner.ProvisionRoot(context.Background(), validRootRequest())
	if resources != nil || !errors.Is(err, primary) || !errors.Is(err, cleanup) {
		t.Fatalf("ProvisionRoot() = (%v, %v), want joined primary and cleanup errors", resources, err)
	}
	calls := strings.Join(client.calls, ",")
	if !strings.Contains(calls, "delete-instance,delete-volume") {
		t.Fatalf("cleanup order = %s, want instance then volume", calls)
	}
}
