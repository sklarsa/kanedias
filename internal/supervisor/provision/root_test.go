package provision

import (
	"context"
	"errors"
	"io"
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
func (client *recordingRootClient) GetStorageVolume(context.Context, string, string) (*api.StorageVolume, error) {
	client.calls = append(client.calls, "get-seed")
	return &api.StorageVolume{Name: "seed"}, nil
}
func (client *recordingRootClient) CopyStorageVolume(context.Context, string, string, string) error {
	client.calls = append(client.calls, "copy-volume")
	return client.copyErr
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
	return &api.InstanceState{StatusCode: api.Running, Network: map[string]api.InstanceStateNetwork{"eth0": {Addresses: []api.InstanceStateNetworkAddress{{Family: "inet", Scope: "global", Address: "10.55.0.9"}}}}}, nil
}

func rootTestConfig() config.Config {
	return config.Config{
		Network:   config.Network{IPv4: "10.55.0.1/24"},
		BaseImage: config.BaseImage{Name: "base", Source: "images:", Image: "ubuntu"},
		Workspace: config.Workspace{Pool: "btrfs", Volume: "seed"},
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
		operationWasSubmitted: func(error) bool { return false },
		newName:               func() (string, error) { return "session-test", nil },
		readinessTimeout:      time.Second,
		retryInterval:         time.Millisecond,
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

	resources, err := provisioner.ProvisionRoot(context.Background(), RootRequest{SessionID: "root-1", SocketPath: "/tmp/root.sock"})
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

	resources, err := provisioner.ProvisionRoot(context.Background(), RootRequest{SessionID: "root-1", SocketPath: "/tmp/root.sock"})
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
	if client.request.Devices["root"]["pool"] != resources.Pool || client.request.Devices["workspace"]["pool"] != resources.Pool {
		t.Fatalf("root/workspace devices do not use effective pool %q: %#v", resources.Pool, client.request.Devices)
	}
	calls := strings.Join(client.calls, ",")
	if strings.Index(calls, "create-instance") > strings.Index(calls, "start-instance") || device == nil {
		t.Fatalf("instance started before proxy device was submitted: %s", calls)
	}
}

func TestRootProvisionerCleansSubmittedAmbiguityAndJoinsCleanupError(t *testing.T) {
	primary := errors.New("create wait failed")
	cleanup := errors.New("delete instance failed")
	client := &recordingRootClient{createErr: primary, deleteInstErr: cleanup}
	deps := testRootDependencies(client)
	deps.operationWasSubmitted = func(err error) bool { return errors.Is(err, primary) }
	provisioner := newRootProvisioner(rootTestConfig(), deps)

	resources, err := provisioner.ProvisionRoot(context.Background(), RootRequest{SessionID: "root-1", SocketPath: "/tmp/root.sock"})
	if resources != nil || !errors.Is(err, primary) || !errors.Is(err, cleanup) {
		t.Fatalf("ProvisionRoot() = (%v, %v), want joined primary and cleanup errors", resources, err)
	}
	calls := strings.Join(client.calls, ",")
	if !strings.Contains(calls, "delete-instance,delete-volume") {
		t.Fatalf("cleanup order = %s, want instance then volume", calls)
	}
}
