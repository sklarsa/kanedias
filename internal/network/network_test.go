package network

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/sklarsa/kanedias/internal/config"
)

type runnerResult struct {
	output []byte
	err    error
}

type fakeRunner struct {
	calls   [][]string
	results []runnerResult
}

func (f *fakeRunner) Run(_ context.Context, args ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string(nil), args...))
	if len(f.results) == 0 {
		return nil, errors.New("unexpected runner call")
	}
	result := f.results[0]
	f.results = f.results[1:]
	return result.output, result.err
}

func testConfig(ipv6 string) config.Config {
	return config.Config{Network: config.Network{
		IPv4: "10.76.111.1/24",
		IPv6: ipv6,
	}}
}

var lookupCall = []string{"network", "list", "name=kanedias", "--format=json"}

func TestEnsureCreatesMissingBridge(t *testing.T) {
	fake := &fakeRunner{results: []runnerResult{{output: []byte("[]")}, {}}}

	if err := ensure(context.Background(), fake, testConfig("")); err != nil {
		t.Fatalf("ensure() error = %v", err)
	}

	wantCalls := [][]string{
		lookupCall,
		{"network", "create", "kanedias", "--type=bridge", "ipv4.address=10.76.111.1/24"},
	}
	if !reflect.DeepEqual(fake.calls, wantCalls) {
		t.Fatalf("runner calls = %#v, want %#v", fake.calls, wantCalls)
	}
}

func TestEnsureCreatesMissingDualStackBridge(t *testing.T) {
	fake := &fakeRunner{results: []runnerResult{{output: []byte("[]")}, {}}}

	if err := ensure(context.Background(), fake, testConfig("fd42:28e2:2375:7000::1/64")); err != nil {
		t.Fatalf("ensure() error = %v", err)
	}

	wantCalls := [][]string{
		lookupCall,
		{
			"network", "create", "kanedias", "--type=bridge",
			"ipv4.address=10.76.111.1/24",
			"ipv6.address=fd42:28e2:2375:7000::1/64",
		},
	}
	if !reflect.DeepEqual(fake.calls, wantCalls) {
		t.Fatalf("runner calls = %#v, want %#v", fake.calls, wantCalls)
	}
}

func TestEnsureAcceptsMatchingBridge(t *testing.T) {
	fake := &fakeRunner{results: []runnerResult{{output: []byte(`[{"name":"kanedias","type":"bridge","managed":true,"config":{"ipv4.address":"10.76.111.1/24","ipv6.address":"fd42:28e2:2375:7000:0:0:0:1/64"}}]`)}}}

	if err := ensure(context.Background(), fake, testConfig("fd42:28e2:2375:7000::1/64")); err != nil {
		t.Fatalf("ensure() error = %v", err)
	}
	assertCalls(t, fake, lookupCall)
}

func TestEnsureRejectsUnmanagedNetwork(t *testing.T) {
	fake := listRunner(`[{"name":"kanedias","type":"bridge","managed":false,"config":{"ipv4.address":"10.76.111.1/24"}}]`)

	err := ensure(context.Background(), fake, testConfig(""))
	assertErrorContains(t, err, "not managed")
	assertCalls(t, fake, lookupCall)
}

func TestEnsureRejectsNonBridgeNetwork(t *testing.T) {
	fake := listRunner(`[{"name":"kanedias","type":"physical","managed":true,"config":{"ipv4.address":"10.76.111.1/24"}}]`)

	err := ensure(context.Background(), fake, testConfig(""))
	assertErrorContains(t, err, "must be a bridge")
	assertCalls(t, fake, lookupCall)
}

func TestEnsureRejectsDifferentIPv4Address(t *testing.T) {
	fake := listRunner(`[{"name":"kanedias","type":"bridge","managed":true,"config":{"ipv4.address":"10.76.112.1/24"}}]`)

	err := ensure(context.Background(), fake, testConfig(""))
	assertErrorContains(t, err, "10.76.112.1/24", "10.76.111.1/24")
	assertCalls(t, fake, lookupCall)
}

func TestEnsureRejectsDifferentConfiguredIPv6Address(t *testing.T) {
	fake := listRunner(`[{"name":"kanedias","type":"bridge","managed":true,"config":{"ipv4.address":"10.76.111.1/24","ipv6.address":"fd42:28e2:2375:8000::1/64"}}]`)

	err := ensure(context.Background(), fake, testConfig("fd42:28e2:2375:7000::1/64"))
	assertErrorContains(t, err, "fd42:28e2:2375:8000::1/64", "fd42:28e2:2375:7000::1/64")
	assertCalls(t, fake, lookupCall)
}

func TestEnsureIgnoresExistingIPv6WhenUnconfigured(t *testing.T) {
	fake := listRunner(`[{"name":"kanedias","type":"bridge","managed":true,"config":{"ipv4.address":"10.76.111.1/24","ipv6.address":"fd42:ffff::1/48"}}]`)

	if err := ensure(context.Background(), fake, testConfig("")); err != nil {
		t.Fatalf("ensure() error = %v", err)
	}
	assertCalls(t, fake, lookupCall)
}

func TestEnsureRejectsMalformedListJSON(t *testing.T) {
	fake := listRunner(`not-json`)

	err := ensure(context.Background(), fake, testConfig(""))
	assertErrorContains(t, err, "decode Incus network list")
	assertCalls(t, fake, lookupCall)
}

func TestEnsurePropagatesListErrorWithoutCreating(t *testing.T) {
	listErr := errors.New("list failed")
	fake := &fakeRunner{results: []runnerResult{{err: listErr}}}

	err := ensure(context.Background(), fake, testConfig(""))
	if !errors.Is(err, listErr) {
		t.Fatalf("ensure() error = %v, want error wrapping %v", err, listErr)
	}
	assertCalls(t, fake, lookupCall)
}

func TestEnsurePropagatesCreateError(t *testing.T) {
	createErr := errors.New("create failed")
	fake := &fakeRunner{results: []runnerResult{{output: []byte("[]")}, {err: createErr}}}

	err := ensure(context.Background(), fake, testConfig(""))
	if !errors.Is(err, createErr) {
		t.Fatalf("ensure() error = %v, want error wrapping %v", err, createErr)
	}
	wantCalls := [][]string{
		lookupCall,
		{"network", "create", "kanedias", "--type=bridge", "ipv4.address=10.76.111.1/24"},
	}
	if !reflect.DeepEqual(fake.calls, wantCalls) {
		t.Fatalf("runner calls = %#v, want %#v", fake.calls, wantCalls)
	}
}

func TestEnsureValidatesDirectConfigBeforeRunnerCall(t *testing.T) {
	tests := []struct {
		name    string
		cfg     config.Config
		wantErr string
	}{
		{name: "missing IPv4", cfg: config.Config{}, wantErr: "network.ipv4"},
		{name: "invalid IPv4", cfg: config.Config{Network: config.Network{IPv4: "bad"}}, wantErr: "network.ipv4"},
		{name: "invalid IPv6", cfg: config.Config{Network: config.Network{IPv4: "10.76.111.1/24", IPv6: "bad"}}, wantErr: "network.ipv6"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeRunner{}
			err := ensure(context.Background(), fake, tt.cfg)
			assertErrorContains(t, err, tt.wantErr)
			if len(fake.calls) != 0 {
				t.Fatalf("runner calls = %#v, want none", fake.calls)
			}
		})
	}
}

func TestEnsureRejectsMultipleExactNameResults(t *testing.T) {
	fake := listRunner(`[
		{"name":"kanedias","type":"bridge","managed":true,"config":{"ipv4.address":"10.76.111.1/24"}},
		{"name":"kanedias","type":"bridge","managed":true,"config":{"ipv4.address":"10.76.111.1/24"}}
	]`)

	err := ensure(context.Background(), fake, testConfig(""))
	assertErrorContains(t, err, "multiple", "kanedias")
	assertCalls(t, fake, lookupCall)
}

func listRunner(output string) *fakeRunner {
	return &fakeRunner{results: []runnerResult{{output: []byte(output)}}}
}

func assertCalls(t *testing.T, fake *fakeRunner, calls ...[]string) {
	t.Helper()
	if !reflect.DeepEqual(fake.calls, calls) {
		t.Fatalf("runner calls = %#v, want %#v", fake.calls, calls)
	}
}

func assertErrorContains(t *testing.T, err error, values ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("ensure() error = nil, want error containing %q", values)
	}
	for _, value := range values {
		if !strings.Contains(err.Error(), value) {
			t.Errorf("ensure() error = %q, want it to contain %q", err, value)
		}
	}
}
