package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lxc/incus/v7/shared/api"
	"github.com/sklarsa/kanedias/internal/config"
	"github.com/sklarsa/kanedias/internal/incusclient"
)

type cleanupObservation struct {
	err       error
	deadline  bool
	remaining time.Duration
}

type workspaceExecResponse struct {
	stdout string
	stderr string
	err    error
}

type fakeClient struct {
	calls               []string
	createRequest       api.InstancesPost
	storageFound        bool
	cancel              context.CancelFunc
	requestCtx          context.Context
	dnsCtxBounded       bool
	readinessCtxBounded bool
	systemdResponses    []workspaceExecResponse
	cleanupCtxs         []cleanupObservation
}

func (f *fakeClient) observeCleanup(ctx context.Context) {
	deadline, ok := ctx.Deadline()
	f.cleanupCtxs = append(f.cleanupCtxs, cleanupObservation{
		err:       ctx.Err(),
		deadline:  ok,
		remaining: time.Until(deadline),
	})
}

func (f *fakeClient) record(call string) { f.calls = append(f.calls, call) }
func (f *fakeClient) Disconnect()        { f.record("disconnect") }

func (f *fakeClient) ResolvePool(ctx context.Context, configured string) (string, error) {
	f.record("resolve-pool")
	return "pool", nil
}

func (f *fakeClient) GetStorageVolume(ctx context.Context, pool, name string) (*api.StorageVolume, error) {
	f.record("get-seed")
	if !f.storageFound {
		return nil, api.StatusErrorf(http.StatusNotFound, "missing")
	}
	return &api.StorageVolume{Name: name}, nil
}

func (f *fakeClient) CreateStorageVolume(ctx context.Context, pool, name string) error {
	f.record("create-seed")
	return nil
}

func (f *fakeClient) GetNetwork(context.Context, string) (*api.Network, error) {
	panic("network calls are replaced by the test dependency")
}

func (f *fakeClient) CreateNetwork(context.Context, api.NetworksPost) error {
	panic("network calls are replaced by the test dependency")
}

func (f *fakeClient) EnsureProfile(ctx context.Context, name string, definition []byte) error {
	f.record("ensure-profile " + name)
	return nil
}

func (f *fakeClient) CreateInstance(ctx context.Context, request api.InstancesPost) error {
	f.createRequest = request
	device := request.Devices[workspaceDevice]
	f.record(fmt.Sprintf("create-instance %s %s %s", request.Source.Alias, device["pool"], device["source"]))
	return nil
}

func (f *fakeClient) StartInstance(ctx context.Context, name string) error {
	f.record("start")
	return nil
}

func (f *fakeClient) StopInstance(ctx context.Context, name string, force bool) error {
	f.record("stop")
	f.observeCleanup(ctx)
	return nil
}

func (f *fakeClient) GetInstance(ctx context.Context, name string) (*api.Instance, string, error) {
	f.record("get-instance")
	f.observeCleanup(ctx)
	return &api.Instance{InstancePut: api.InstancePut{Devices: api.DevicesMap{
		workspaceDevice: {"type": "disk", "pool": "pool", "source": "seed", "path": workspacePath},
	}}}, "etag", nil
}

func (f *fakeClient) UpdateInstance(ctx context.Context, name string, request api.InstancePut, etag string) error {
	if _, present := request.Devices[workspaceDevice]; present {
		return errors.New("workspace device was not removed")
	}
	f.record("update-instance")
	f.observeCleanup(ctx)
	return nil
}

func (f *fakeClient) DeleteInstance(ctx context.Context, name string) error {
	f.record("delete-instance")
	f.observeCleanup(ctx)
	return nil
}

func (f *fakeClient) Exec(ctx context.Context, name string, request incusclient.ExecRequest) (string, string, error) {
	command := strings.Join(request.Command, " ")
	f.record("exec " + command)
	isDNS := reflect.DeepEqual(request.Command, []string{"getent", "ahosts", "github.com"})
	isSystemd := reflect.DeepEqual(request.Command, []string{"systemctl", "is-system-running", "--wait"})
	switch {
	case isDNS:
		deadline, ok := ctx.Deadline()
		f.dnsCtxBounded = ok && time.Until(deadline) > 0 && time.Until(deadline) <= dnsTimeout
	case isSystemd:
		deadline, ok := ctx.Deadline()
		f.readinessCtxBounded = ok && time.Until(deadline) > 0 && time.Until(deadline) <= systemdTimeout
		if len(f.systemdResponses) > 0 {
			response := f.systemdResponses[0]
			if len(f.systemdResponses) > 1 {
				f.systemdResponses = f.systemdResponses[1:]
			}
			return response.stdout, response.stderr, response.err
		}
		return "running\n", "", nil
	case f.requestCtx != nil && ctx != f.requestCtx:
		return "", "", errors.New("exec did not receive request context")
	}
	if isDNS && f.cancel != nil {
		f.cancel()
		return "", "", context.Canceled
	}
	if strings.Contains(command, "test -e /workspace/repos/new") {
		return "", "", errors.New("exit status 1")
	}
	if strings.Contains(command, "rev-parse --show-toplevel") {
		parts := strings.Split(command, " ")
		for i, part := range parts {
			if part == "-C" && i+1 < len(parts) {
				return parts[i+1] + "\n", "", nil
			}
		}
	}
	if strings.Contains(command, "symbolic-ref refs/remotes/origin/HEAD") {
		return "refs/remotes/origin/main\n", "", nil
	}
	return "", "", nil
}

func testConfig(repos ...string) config.Config {
	return config.Config{
		BaseImage: config.BaseImage{
			Name:   "base",
			Source: "https://images.linuxcontainers.org",
			Image:  "debian/13",
		},
		Workspace: config.Workspace{Pool: "pool", Volume: "seed", Repos: repos},
	}
}

func testDependencies(fake *fakeClient) dependencies {
	return dependencies{
		connect: func(context.Context) (client, error) { return fake, nil },
		initCA: func() error {
			fake.record("init-ca")
			return nil
		},
		ensureNetwork: func(context.Context, client, config.Config) error {
			fake.record("ensure-network")
			return nil
		},
		renderProfile:         func(io.Writer, string, config.Config) error { return nil },
		readinessTimeout:      systemdTimeout,
		readinessPollInterval: time.Nanosecond,
	}
}

func TestSyncValidatesEveryRequiredLifecycleFieldBeforeSideEffects(t *testing.T) {
	for _, tt := range []struct {
		name       string
		invalidate func(*config.Config)
		want       string
	}{
		{name: "name", invalidate: func(cfg *config.Config) { cfg.BaseImage.Name = "" }, want: "base_image.name is required"},
		{name: "source", invalidate: func(cfg *config.Config) { cfg.BaseImage.Source = "" }, want: "base_image.source is required"},
		{name: "image", invalidate: func(cfg *config.Config) { cfg.BaseImage.Image = "" }, want: "base_image.image is required"},
	} {
		t.Run("missing-"+tt.name, func(t *testing.T) {
			cfg := testConfig()
			tt.invalidate(&cfg)
			connected := false
			deps := dependencies{connect: func(context.Context) (client, error) {
				connected = true
				return nil, errors.New("unexpected connection")
			}}
			err := syncWithDependencies(context.Background(), cfg, io.Discard, io.Discard, deps)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want containing %q", err, tt.want)
			}
			if connected {
				t.Fatal("connected before validating lifecycle config")
			}
		})
	}
}

func TestSyncValidatesRepositoriesBeforeConnecting(t *testing.T) {
	tests := []struct {
		name  string
		repos []string
		want  string
	}{
		{name: "missing owner", repos: []string{"repo"}, want: "invalid GitHub repository slug"},
		{name: "missing repository", repos: []string{"owner/"}, want: "invalid GitHub repository slug"},
		{name: "extra separator", repos: []string{"owner/repo/extra"}, want: "invalid GitHub repository slug"},
		{name: "whitespace", repos: []string{"owner/my repo"}, want: "invalid GitHub repository slug"},
		{name: "duplicate basename", repos: []string{"one/repo", "two/repo"}, want: "duplicate repository destination"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			connected := false
			deps := dependencies{connect: func(context.Context) (client, error) {
				connected = true
				return nil, errors.New("unexpected connection")
			}}
			err := syncWithDependencies(context.Background(), testConfig(tt.repos...), io.Discard, io.Discard, deps)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want containing %q", err, tt.want)
			}
			if connected {
				t.Fatal("connected before validating repositories")
			}
		})
	}
}

func TestSyncEmptyRepositoriesEnsuresSeedAndWarns(t *testing.T) {
	for _, tt := range []struct {
		name         string
		storageFound bool
		want         []string
	}{
		{name: "create", want: []string{"resolve-pool", "get-seed", "create-seed", "disconnect"}},
		{name: "reuse", storageFound: true, want: []string{"resolve-pool", "get-seed", "disconnect"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeClient{storageFound: tt.storageFound}
			var stderr bytes.Buffer
			if err := syncWithDependencies(context.Background(), testConfig(), io.Discard, &stderr, testDependencies(fake)); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(fake.calls, tt.want) {
				t.Fatalf("calls = %#v, want %#v", fake.calls, tt.want)
			}
			if !strings.Contains(stderr.String(), "no repositories configured") {
				t.Fatalf("stderr = %q, want empty-list warning", stderr.String())
			}
		})
	}
}

type workspaceTestCtxKey struct{}

func TestSyncRetriesSystemdBeforeCAAndDNSAndDestructiveRefresh(t *testing.T) {
	ctx := context.WithValue(context.Background(), workspaceTestCtxKey{}, "request")
	fake := &fakeClient{
		requestCtx: ctx,
		systemdResponses: []workspaceExecResponse{
			{stderr: "Failed to connect to system scope bus via local transport: No such file or directory", err: errors.New("exit status 1")},
			{stdout: "running\n"},
		},
	}
	if err := syncWithDependencies(ctx, testConfig("one/existing", "two/new"), io.Discard, io.Discard, testDependencies(fake)); err != nil {
		t.Fatal(err)
	}

	ordered := []string{
		"resolve-pool", "get-seed", "create-seed", "init-ca", "ensure-network", "ensure-profile sandbox",
		"create-instance base pool seed", "start",
		"exec systemctl is-system-running --wait", "exec systemctl is-system-running --wait",
		"exec update-ca-certificates", "exec getent ahosts github.com",
		"exec install -d -o kanedias -g kanedias /workspace/repos",
		"exec runuser -u kanedias -- env HOME=/home/kanedias USER=kanedias LOGNAME=kanedias gh auth setup-git --hostname github.com --force",
		"exec runuser -u kanedias -- env HOME=/home/kanedias USER=kanedias LOGNAME=kanedias git config --global --replace-all url.https://github.com/.insteadOf git@github.com:",
		"exec runuser -u kanedias -- env HOME=/home/kanedias USER=kanedias LOGNAME=kanedias git config --global --add url.https://github.com/.insteadOf ssh://git@github.com/",
		"exec runuser -u kanedias -- env HOME=/home/kanedias USER=kanedias LOGNAME=kanedias gh repo clone https://github.com/two/new.git /workspace/repos/new -- --recurse-submodules",
		"stop", "get-instance", "update-instance", "delete-instance", "disconnect",
	}
	assertOrdered(t, fake.calls, ordered)
	if !fake.readinessCtxBounded {
		t.Fatal("systemd exec did not receive a bounded request-derived context")
	}
	if !fake.dnsCtxBounded {
		t.Fatal("DNS exec did not receive a bounded request-derived context")
	}
	wantRoot := map[string]string{"type": "disk", "pool": "pool", "path": "/"}
	if got := fake.createRequest.Devices["root"]; !reflect.DeepEqual(got, wantRoot) {
		t.Fatalf("root device = %#v, want %#v", got, wantRoot)
	}

	commands := strings.Join(fake.calls, "\n")
	for _, want := range []string{
		"git -C /workspace/repos/existing fetch --force --prune --prune-tags --tags origin",
		"git -C /workspace/repos/existing remote set-head origin --auto",
		"git -C /workspace/repos/existing symbolic-ref refs/remotes/origin/HEAD",
		"git -C /workspace/repos/existing checkout --force -B main refs/remotes/origin/main",
		"git -C /workspace/repos/existing reset --hard refs/remotes/origin/main",
		"git -C /workspace/repos/existing clean -ffdx",
		"git -C /workspace/repos/existing submodule sync --recursive",
		"git -C /workspace/repos/existing submodule update --init --recursive --force",
		"git -C /workspace/repos/existing submodule foreach --recursive git reset --hard && git clean -ffdx",
	} {
		if !strings.Contains(commands, want) {
			t.Errorf("missing destructive refresh command %q\ncalls:\n%s", want, commands)
		}
	}
}

func TestSyncReadinessTimeoutPreventsCAUpdateAndDNS(t *testing.T) {
	fake := &fakeClient{
		storageFound: true,
		systemdResponses: []workspaceExecResponse{{
			stderr: "system bus unavailable",
			err:    errors.New("exit status 1"),
		}},
	}
	deps := testDependencies(fake)
	deps.readinessTimeout = 10 * time.Millisecond
	deps.readinessPollInterval = time.Millisecond

	err := syncWithDependencies(context.Background(), testConfig("one/repo"), io.Discard, io.Discard, deps)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Sync error = %v, want systemd readiness deadline", err)
	}
	if countWorkspaceCall(fake.calls, "exec systemctl is-system-running --wait") < 2 {
		t.Fatalf("systemd calls did not retry by condition: %v", fake.calls)
	}
	for _, forbidden := range []string{"exec update-ca-certificates", "exec getent ahosts github.com", "exec install -d -o kanedias -g kanedias /workspace/repos"} {
		if containsWorkspaceCall(fake.calls, forbidden) {
			t.Fatalf("call %q occurred before systemd readiness: %v", forbidden, fake.calls)
		}
	}
}

func TestSyncAcceptsDegradedSystemdBeforeCAUpdate(t *testing.T) {
	fake := &fakeClient{storageFound: true, systemdResponses: []workspaceExecResponse{{
		stdout: "degraded\n",
		err:    errors.New("systemd degraded exit status"),
	}}}
	if err := syncWithDependencies(context.Background(), testConfig("one/repo"), io.Discard, io.Discard, testDependencies(fake)); err != nil {
		t.Fatal(err)
	}
	assertOrdered(t, fake.calls, []string{
		"exec systemctl is-system-running --wait",
		"exec update-ca-certificates",
		"exec getent ahosts github.com",
	})
}

func TestSyncCancellationUsesBoundedNonCancelledCleanupContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	fake := &fakeClient{storageFound: true, cancel: cancel}
	err := syncWithDependencies(ctx, testConfig("one/repo"), io.Discard, io.Discard, testDependencies(fake))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context cancellation", err)
	}
	assertOrdered(t, fake.calls, []string{"start", "exec getent ahosts github.com", "stop", "get-instance", "update-instance", "delete-instance"})
	if len(fake.cleanupCtxs) != 4 {
		t.Fatalf("cleanup contexts = %d, want 4", len(fake.cleanupCtxs))
	}
	for _, cleanupCtx := range fake.cleanupCtxs {
		if cleanupCtx.err != nil {
			t.Errorf("cleanup context was cancelled: %v", cleanupCtx.err)
		}
		if !cleanupCtx.deadline {
			t.Error("cleanup context has no deadline")
		} else if cleanupCtx.remaining <= 0 || cleanupCtx.remaining > cleanupTimeout {
			t.Errorf("cleanup deadline remaining = %v, want (0, %v]", cleanupCtx.remaining, cleanupTimeout)
		}
	}
}

func countWorkspaceCall(calls []string, want string) int {
	count := 0
	for _, call := range calls {
		if call == want {
			count++
		}
	}
	return count
}

func containsWorkspaceCall(calls []string, want string) bool {
	for _, call := range calls {
		if call == want {
			return true
		}
	}
	return false
}

func assertOrdered(t *testing.T, calls, want []string) {
	t.Helper()
	next := 0
	for _, call := range calls {
		if next < len(want) && call == want[next] {
			next++
		}
	}
	if next != len(want) {
		t.Fatalf("calls missing ordered item %q at index %d\ncalls:\n%s", want[next], next, strings.Join(calls, "\n"))
	}
}
