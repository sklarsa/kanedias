package network

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/lxc/incus/v7/shared/api"
	"github.com/sklarsa/kanedias/internal/config"
)

type fakeClient struct {
	network       *api.Network
	getErr        error
	createErr     error
	getCalls      int
	createCalls   int
	created       api.NetworksPost
	getContext    context.Context
	createContext context.Context
}

func (f *fakeClient) GetNetwork(ctx context.Context, _ string) (*api.Network, error) {
	f.getCalls++
	f.getContext = ctx
	return f.network, f.getErr
}
func (f *fakeClient) CreateNetwork(ctx context.Context, network api.NetworksPost) error {
	f.createCalls++
	f.createContext = ctx
	f.created = network
	return f.createErr
}

func testConfig(ipv6 string) config.Config {
	return config.Config{Network: config.Network{IPv4: "10.76.111.1/24", IPv6: ipv6}}
}

func missingClient() *fakeClient {
	return &fakeClient{getErr: api.StatusErrorf(http.StatusNotFound, "missing")}
}

func TestEnsureWithClientCreatesMissingBridge(t *testing.T) {
	ctx := context.WithValue(context.Background(), struct{}{}, "request")
	fake := missingClient()
	if err := EnsureWithClient(ctx, fake, testConfig("")); err != nil {
		t.Fatal(err)
	}
	if fake.getCalls != 1 || fake.createCalls != 1 {
		t.Fatalf("calls = get %d, create %d", fake.getCalls, fake.createCalls)
	}
	if fake.getContext != ctx || fake.createContext != ctx {
		t.Fatal("network requests did not receive supplied context")
	}
	if fake.created.Name != Name || fake.created.Type != "bridge" {
		t.Fatalf("created network = %#v", fake.created)
	}
	if got := fake.created.Config["ipv4.address"]; got != "10.76.111.1/24" {
		t.Fatalf("ipv4.address = %q", got)
	}
	if got := fake.created.Config["ipv4.nat"]; got != "true" {
		t.Fatalf("ipv4.nat = %q, want true", got)
	}
	if _, exists := fake.created.Config["ipv6.address"]; exists {
		t.Fatal("unexpected ipv6.address")
	}
}

func TestEnsureWithClientCreatesMissingDualStackBridge(t *testing.T) {
	fake := missingClient()
	if err := EnsureWithClient(context.Background(), fake, testConfig("fd42:28e2:2375:7000::1/64")); err != nil {
		t.Fatal(err)
	}
	if got := fake.created.Config["ipv6.address"]; got != "fd42:28e2:2375:7000::1/64" {
		t.Fatalf("ipv6.address = %q", got)
	}
}

func TestEnsureWithClientAcceptsMatchingBridge(t *testing.T) {
	fake := &fakeClient{network: bridge(true, "bridge", "10.76.111.1/24", "fd42:28e2:2375:7000:0:0:0:1/64")}
	if err := EnsureWithClient(context.Background(), fake, testConfig("fd42:28e2:2375:7000::1/64")); err != nil {
		t.Fatal(err)
	}
	if fake.createCalls != 0 {
		t.Fatal("matching network was recreated")
	}
}

func TestEnsureWithClientRejectsExistingBridgeWithoutIPv4NAT(t *testing.T) {
	network := bridge(true, "bridge", "10.76.111.1/24", "")
	delete(network.Config, "ipv4.nat")

	err := EnsureWithClient(context.Background(), &fakeClient{network: network}, testConfig(""))
	assertErrorContains(t, err,
		"ipv4.nat",
		"true",
		"incus network set kanedias ipv4.nat=true --project default",
	)
}

func TestEnsureWithClientRejectsInvalidExistingNetwork(t *testing.T) {
	tests := []struct {
		name    string
		network *api.Network
		cfg     config.Config
		want    []string
	}{
		{name: "unmanaged", network: bridge(false, "bridge", "10.76.111.1/24", ""), cfg: testConfig(""), want: []string{"not managed"}},
		{name: "non-bridge", network: bridge(true, "physical", "10.76.111.1/24", ""), cfg: testConfig(""), want: []string{"must be a bridge"}},
		{name: "IPv4 mismatch", network: bridge(true, "bridge", "10.76.112.1/24", ""), cfg: testConfig(""), want: []string{"10.76.112.1/24", "10.76.111.1/24"}},
		{name: "IPv6 mismatch", network: bridge(true, "bridge", "10.76.111.1/24", "fd42:28e2:2375:8000::1/64"), cfg: testConfig("fd42:28e2:2375:7000::1/64"), want: []string{"fd42:28e2:2375:8000::1/64", "fd42:28e2:2375:7000::1/64"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := EnsureWithClient(context.Background(), &fakeClient{network: tt.network}, tt.cfg)
			assertErrorContains(t, err, tt.want...)
		})
	}
}

func TestEnsureWithClientIgnoresExistingIPv6WhenUnconfigured(t *testing.T) {
	fake := &fakeClient{network: bridge(true, "bridge", "10.76.111.1/24", "fd42:ffff::1/48")}
	if err := EnsureWithClient(context.Background(), fake, testConfig("")); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureWithClientPropagatesClientErrors(t *testing.T) {
	getErr := errors.New("get failed")
	if err := EnsureWithClient(context.Background(), &fakeClient{getErr: getErr}, testConfig("")); !errors.Is(err, getErr) {
		t.Fatalf("get error = %v", err)
	}

	createErr := errors.New("create failed")
	fake := missingClient()
	fake.createErr = createErr
	if err := EnsureWithClient(context.Background(), fake, testConfig("")); !errors.Is(err, createErr) {
		t.Fatalf("create error = %v", err)
	}
}

func TestEnsureWithClientValidatesConfigBeforeClientCall(t *testing.T) {
	tests := []struct {
		name string
		cfg  config.Config
		want string
	}{
		{name: "missing IPv4", cfg: config.Config{}, want: "network.ipv4"},
		{name: "invalid IPv4", cfg: config.Config{Network: config.Network{IPv4: "bad"}}, want: "network.ipv4"},
		{name: "invalid IPv6", cfg: config.Config{Network: config.Network{IPv4: "10.76.111.1/24", IPv6: "bad"}}, want: "network.ipv6"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeClient{}
			assertErrorContains(t, EnsureWithClient(context.Background(), fake, tt.cfg), tt.want)
			if fake.getCalls != 0 {
				t.Fatalf("get calls = %d, want 0", fake.getCalls)
			}
		})
	}
}

func bridge(managed bool, networkType, ipv4, ipv6 string) *api.Network {
	config := api.ConfigMap{"ipv4.address": ipv4, "ipv4.nat": "true"}
	if ipv6 != "" {
		config["ipv6.address"] = ipv6
	}
	return &api.Network{Name: Name, Type: networkType, Managed: managed, NetworkPut: api.NetworkPut{Config: config}}
}

func assertErrorContains(t *testing.T, err error, values ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want error containing %q", values)
	}
	for _, value := range values {
		if !strings.Contains(err.Error(), value) {
			t.Errorf("error = %q, want it to contain %q", err, value)
		}
	}
}
