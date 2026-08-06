package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lxc/incus/v7/shared/api"
	"github.com/sklarsa/kanedias/internal/config"
	"github.com/sklarsa/kanedias/internal/incusclient"
	"github.com/sklarsa/kanedias/internal/proxy"
)

func TestCreateOrdersLifecycleAndBuildsOwnedWorkspaceDevice(t *testing.T) {
	fake := &recordingClient{}
	deps := testDependencies(fake)

	if err := create(context.Background(), testConfig(), "demo", io.Discard, io.Discard, deps); err != nil {
		t.Fatal(err)
	}

	wantCalls := []string{
		"resolve-pool", "lock", "init-ca", "ensure-network", "ensure-profile sandbox",
		"get-volume kanedias-workspace-seed", "copy-volume kanedias-workspace-seed kanedias-workspace-demo",
		"create-instance", "start", "exec systemctl is-system-running --wait", "exec update-ca-certificates",
	}
	assertCalls(t, fake.calls, wantCalls)

	request := fake.createRequest
	if request.Name != "demo" {
		t.Fatalf("instance name = %q, want demo", request.Name)
	}
	if request.Source.Type != "image" || request.Source.Alias != "kanedias-base" {
		t.Fatalf("instance source = %#v, want local kanedias-base alias", request.Source)
	}
	if got := strings.Join(request.Profiles, ","); got != "default,sandbox" {
		t.Fatalf("profiles = %q, want default,sandbox", got)
	}
	wantDevice := map[string]string{
		"type": "disk", "pool": "pool1", "source": "kanedias-workspace-demo", "path": "/workspace",
	}
	if got := request.Devices["workspace"]; !equalStringMap(got, wantDevice) {
		t.Fatalf("workspace device = %#v, want %#v", got, wantDevice)
	}
	wantRoot := map[string]string{"type": "disk", "pool": "pool1", "path": "/"}
	if got := request.Devices["root"]; !equalStringMap(got, wantRoot) {
		t.Fatalf("root device = %#v, want %#v", got, wantRoot)
	}
	if remaining := time.Until(fake.readinessDeadline); remaining < 59*time.Second || remaining > 61*time.Second {
		t.Fatalf("systemd readiness deadline is %v away, want about 60s", remaining)
	}
}

func TestCreateRetriesPreBusSystemdFailureUntilRunning(t *testing.T) {
	fake := &recordingClient{systemdResponses: []execResponse{
		{stderr: "Failed to connect to system scope bus via local transport: No such file or directory", err: errors.New("exit status 1")},
		{stdout: "running\n"},
	}}
	if err := create(context.Background(), testConfig(), "demo", io.Discard, io.Discard, testDependencies(fake)); err != nil {
		t.Fatal(err)
	}
	if got := countCall(fake.calls, "exec systemctl is-system-running --wait"); got != 2 {
		t.Fatalf("systemd readiness calls = %d, want 2", got)
	}
	assertOrderedCalls(t, fake.calls, []string{
		"exec systemctl is-system-running --wait",
		"exec systemctl is-system-running --wait",
		"exec update-ca-certificates",
	})
}

func TestCreateRetriesPreBusSystemdFailureUntilDegraded(t *testing.T) {
	fake := &recordingClient{systemdResponses: []execResponse{
		{stderr: "Failed to connect to system scope bus via local transport: No such file or directory", err: errors.New("exit status 1")},
		{stdout: "degraded\n", err: errors.New("systemd degraded exit status")},
	}}
	if err := create(context.Background(), testConfig(), "demo", io.Discard, io.Discard, testDependencies(fake)); err != nil {
		t.Fatal(err)
	}
}

func TestWaitForSystemdTimesOutWhileConditionRemainsFalse(t *testing.T) {
	fake := &recordingClient{systemdResponses: []execResponse{{stderr: "system bus unavailable", err: errors.New("exit status 1")}}}
	err := waitForSystemd(context.Background(), fake, "demo", 10*time.Millisecond, time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waitForSystemd error = %v, want deadline exceeded", err)
	}
	if got := countCall(fake.calls, "exec systemctl is-system-running --wait"); got < 2 {
		t.Fatalf("systemd readiness calls = %d, want a condition retry", got)
	}
}

func TestWaitForSystemdStopsOnRequestCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	attempted := make(chan struct{}, 1)
	fake := &recordingClient{
		systemdAttempted: attempted,
		systemdResponses: []execResponse{{stderr: "system bus unavailable", err: errors.New("exit status 1")}},
	}
	result := make(chan error, 1)
	go func() {
		result <- waitForSystemd(ctx, fake, "demo", time.Minute, time.Hour)
	}()
	<-attempted
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("waitForSystemd error = %v, want context cancellation", err)
	}
}

func TestCreateUsesRequestDerivedBoundedContextToCleanOwnedResources(t *testing.T) {
	const sentinel = "request-value"
	requestCtx := context.WithValue(context.Background(), requestContextKey{}, sentinel)
	requestCtx, cancel := context.WithCancel(requestCtx)
	fake := &recordingClient{startFunc: func(context.Context) error {
		cancel()
		return nil
	}}
	deps := testDependencies(fake)

	err := create(requestCtx, testConfig(), "demo", io.Discard, io.Discard, deps)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Create error = %v, want context cancellation", err)
	}
	assertCalls(t, fake.calls[len(fake.calls)-3:], []string{"stop", "delete-instance", "delete-volume kanedias-workspace-demo"})
	if len(fake.cleanupContextErrs) != 3 {
		t.Fatalf("cleanup context count = %d, want 3", len(fake.cleanupContextErrs))
	}
	for i, contextErr := range fake.cleanupContextErrs {
		if contextErr != nil {
			t.Fatalf("cleanup used canceled context: %v", contextErr)
		}
		deadline := fake.cleanupDeadlines[i]
		if deadline.IsZero() || time.Until(deadline) > 31*time.Second {
			t.Fatalf("cleanup context does not have the bounded deadline: %v", deadline)
		}
		if got := fake.cleanupContextValues[i]; got != sentinel {
			t.Fatalf("cleanup context value = %v, want %q", got, sentinel)
		}
	}
}

func TestCreateDoesNotDeleteInstanceItDidNotCreate(t *testing.T) {
	fake := &recordingClient{createErr: errors.New("name collision")}
	err := create(context.Background(), testConfig(), "demo", io.Discard, io.Discard, testDependencies(fake))
	if err == nil {
		t.Fatal("Create succeeded after instance collision")
	}
	if containsCall(fake.calls, "delete-instance") {
		t.Fatalf("calls include deletion of unowned instance: %v", fake.calls)
	}
	if !containsCall(fake.calls, "delete-volume kanedias-workspace-demo") {
		t.Fatalf("owned volume was not cleaned up: %v", fake.calls)
	}
}

func TestDestroyStopsRunningVerifiedInstanceBeforeDeletingInstanceAndVolume(t *testing.T) {
	fake := &recordingClient{
		instance: &api.Instance{
			StatusCode: api.Running,
			InstancePut: api.InstancePut{Devices: map[string]map[string]string{
				"workspace": {"source": "kanedias-workspace-demo", "type": "disk"},
			}},
		},
		volume:  &api.StorageVolume{Name: "kanedias-workspace-demo"},
		running: true,
	}
	if err := destroy(context.Background(), testConfig(), "demo", io.Discard, io.Discard, testDependencies(fake)); err != nil {
		t.Fatal(err)
	}
	assertCalls(t, fake.calls[len(fake.calls)-5:], []string{
		"get-instance", "stop", "delete-instance", "get-volume kanedias-workspace-demo", "delete-volume kanedias-workspace-demo",
	})
}

func TestDestroyRefusesUnverifiedWorkspaceWithoutDeletion(t *testing.T) {
	for _, test := range []struct {
		name    string
		devices map[string]map[string]string
	}{
		{name: "missing", devices: map[string]map[string]string{}},
		{name: "mismatched", devices: map[string]map[string]string{"workspace": {"source": "someone-elses-volume"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &recordingClient{instance: &api.Instance{InstancePut: api.InstancePut{Devices: test.devices}}}
			err := destroy(context.Background(), testConfig(), "demo", io.Discard, io.Discard, testDependencies(fake))
			if err == nil {
				t.Fatal("Destroy succeeded without ownership proof")
			}
			if containsCall(fake.calls, "delete-instance") || containsCall(fake.calls, "delete-volume kanedias-workspace-demo") {
				t.Fatalf("Destroy attempted deletion: %v", fake.calls)
			}
		})
	}
}

func TestDestroySucceedsWhenResourcesAreAbsent(t *testing.T) {
	missing := api.StatusErrorf(http.StatusNotFound, "missing")
	fake := &recordingClient{getInstanceErr: missing, getVolumeErr: missing}
	if err := destroy(context.Background(), testConfig(), "demo", io.Discard, io.Discard, testDependencies(fake)); err != nil {
		t.Fatal(err)
	}
	if containsCall(fake.calls, "delete-instance") || containsCall(fake.calls, "delete-volume kanedias-workspace-demo") {
		t.Fatalf("Destroy attempted deletion: %v", fake.calls)
	}
}

func TestDestroyNeverSelectsSeedVolume(t *testing.T) {
	fake := &recordingClient{}
	err := destroy(context.Background(), testConfig(), "seed", io.Discard, io.Discard, testDependencies(fake))
	if err == nil {
		t.Fatal("Destroy accepted a name whose owned volume would be the seed")
	}
	if containsCall(fake.calls, "delete-volume kanedias-workspace-seed") {
		t.Fatalf("Destroy selected seed volume: %v", fake.calls)
	}
}

func TestLifecycleCommandsValidateEveryRequiredFieldBeforeSideEffects(t *testing.T) {
	for _, field := range []struct {
		name       string
		invalidate func(*config.Config)
		want       string
	}{
		{name: "name", invalidate: func(cfg *config.Config) { cfg.BaseImage.Name = "" }, want: "base_image.name is required"},
		{name: "source", invalidate: func(cfg *config.Config) { cfg.BaseImage.Source = "" }, want: "base_image.source is required"},
		{name: "image", invalidate: func(cfg *config.Config) { cfg.BaseImage.Image = "" }, want: "base_image.image is required"},
	} {
		for _, operation := range []struct {
			name string
			run  func(context.Context, config.Config, dependencies) error
		}{
			{name: "create", run: func(ctx context.Context, cfg config.Config, deps dependencies) error {
				return create(ctx, cfg, "demo", io.Discard, io.Discard, deps)
			}},
			{name: "destroy", run: func(ctx context.Context, cfg config.Config, deps dependencies) error {
				return destroy(ctx, cfg, "demo", io.Discard, io.Discard, deps)
			}},
		} {
			t.Run(operation.name+"/missing-"+field.name, func(t *testing.T) {
				cfg := testConfig()
				field.invalidate(&cfg)
				connected := false
				deps := dependencies{connect: func(context.Context) (lifecycleClient, error) {
					connected = true
					return nil, errors.New("unexpected connection")
				}}
				err := operation.run(context.Background(), cfg, deps)
				if err == nil || !strings.Contains(err.Error(), field.want) {
					t.Fatalf("error = %v, want containing %q", err, field.want)
				}
				if connected {
					t.Fatal("connected before validating lifecycle config")
				}
			})
		}
	}
}

func TestLifecycleLockIsNonBlockingAndPrivate(t *testing.T) {
	name := "test-" + strings.ReplaceAll(t.Name(), "/", "-")
	dir := filepath.Join(os.TempDir(), "kanedias-sandbox-locks-"+fmt.Sprint(os.Getuid()))
	first, err := acquireLifecycleLock(name)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = first.Close()
		_ = os.Remove(filepath.Join(dir, name+".lock"))
	}()

	if second, err := acquireLifecycleLock(name); err == nil {
		second.Close()
		t.Fatal("second lifecycle lock acquisition succeeded")
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0700 {
		t.Fatalf("lock directory mode = %04o, want 0700", got)
	}
}

func TestValidateNameMatchesLegacyLifecycleRules(t *testing.T) {
	for _, name := range []string{"", ".", "..", "with/slash"} {
		if err := validateName(name); err == nil {
			t.Errorf("validateName(%q) succeeded", name)
		}
	}
	if err := validateName("sandbox name"); err != nil {
		t.Fatalf("legacy-valid name rejected: %v", err)
	}
}

func testConfig() config.Config {
	return config.Config{
		Network: config.Network{IPv4: "10.75.177.1/24"},
		BaseImage: config.BaseImage{
			Name:   "kanedias-base",
			Source: "https://images.linuxcontainers.org",
			Image:  "debian/13",
		},
		Workspace: config.Workspace{Pool: "pool1", Volume: config.DefaultWorkspaceVolume},
	}
}

func testDependencies(client *recordingClient) dependencies {
	return dependencies{
		connect: func(context.Context) (lifecycleClient, error) { return client, nil },
		acquireLock: func(string) (io.Closer, error) {
			client.calls = append(client.calls, "lock")
			return nopCloser{}, nil
		},
		defaultProxyOptions: func() (proxy.Options, error) {
			return proxy.Options{CACertPath: "ca.crt", CAKeyPath: "ca.key"}, nil
		},
		initCA: func(string, string) error {
			client.calls = append(client.calls, "init-ca")
			return nil
		},
		ensureNetwork: func(context.Context, lifecycleClient, config.Config) error {
			client.calls = append(client.calls, "ensure-network")
			return nil
		},
		readinessTimeout:      60 * time.Second,
		readinessPollInterval: time.Nanosecond,
	}
}

type nopCloser struct{}

func (nopCloser) Close() error { return nil }

type requestContextKey struct{}

type execResponse struct {
	stdout string
	stderr string
	err    error
}

type recordingClient struct {
	calls                []string
	createRequest        api.InstancesPost
	instance             *api.Instance
	volume               *api.StorageVolume
	getInstanceErr       error
	getVolumeErr         error
	createErr            error
	startFunc            func(context.Context) error
	systemdState         string
	systemdErr           error
	systemdResponses     []execResponse
	systemdAttempted     chan<- struct{}
	running              bool
	cleanupContextErrs   []error
	cleanupDeadlines     []time.Time
	cleanupContextValues []any
	readinessDeadline    time.Time
}

func (c *recordingClient) Disconnect() {}

func (c *recordingClient) ResolvePool(context.Context, string) (string, error) {
	c.calls = append(c.calls, "resolve-pool")
	return "pool1", nil
}

func (c *recordingClient) GetNetwork(context.Context, string) (*api.Network, error) {
	panic("GetNetwork should be hidden behind ensureNetwork in these tests")
}

func (c *recordingClient) CreateNetwork(context.Context, api.NetworksPost) error {
	panic("CreateNetwork should be hidden behind ensureNetwork in these tests")
}

func (c *recordingClient) EnsureProfile(_ context.Context, name string, _ []byte) error {
	c.calls = append(c.calls, "ensure-profile "+name)
	return nil
}

func (c *recordingClient) GetStorageVolume(_ context.Context, _, name string) (*api.StorageVolume, error) {
	c.calls = append(c.calls, "get-volume "+name)
	if c.getVolumeErr != nil {
		return nil, c.getVolumeErr
	}
	if c.volume != nil {
		return c.volume, nil
	}
	return &api.StorageVolume{Name: name}, nil
}

func (c *recordingClient) CopyStorageVolume(_ context.Context, _, source, target string) error {
	c.calls = append(c.calls, "copy-volume "+source+" "+target)
	return nil
}

func (c *recordingClient) DeleteStorageVolume(ctx context.Context, _, name string) error {
	c.calls = append(c.calls, "delete-volume "+name)
	c.recordCleanupContext(ctx)
	return nil
}

func (c *recordingClient) GetInstance(context.Context, string) (*api.Instance, string, error) {
	c.calls = append(c.calls, "get-instance")
	return c.instance, "", c.getInstanceErr
}

func (c *recordingClient) CreateInstance(_ context.Context, request api.InstancesPost) error {
	c.calls = append(c.calls, "create-instance")
	c.createRequest = request
	return c.createErr
}

func (c *recordingClient) StartInstance(ctx context.Context, _ string) error {
	c.calls = append(c.calls, "start")
	c.running = true
	if c.startFunc != nil {
		return c.startFunc(ctx)
	}
	return nil
}

func (c *recordingClient) StopInstance(ctx context.Context, _ string, force bool) error {
	c.calls = append(c.calls, "stop")
	if !force {
		return errors.New("sandbox stop was not forced")
	}
	c.recordCleanupContext(ctx)
	c.running = false
	return nil
}

func (c *recordingClient) DeleteInstance(ctx context.Context, _ string) error {
	c.calls = append(c.calls, "delete-instance")
	c.recordCleanupContext(ctx)
	if c.running {
		return errors.New("cannot delete running instance")
	}
	return nil
}

func (c *recordingClient) recordCleanupContext(ctx context.Context) {
	c.cleanupContextErrs = append(c.cleanupContextErrs, ctx.Err())
	deadline, _ := ctx.Deadline()
	c.cleanupDeadlines = append(c.cleanupDeadlines, deadline)
	c.cleanupContextValues = append(c.cleanupContextValues, ctx.Value(requestContextKey{}))
}

func (c *recordingClient) Exec(ctx context.Context, _ string, request incusclient.ExecRequest) (string, string, error) {
	command := strings.Join(request.Command, " ")
	c.calls = append(c.calls, "exec "+command)
	if command == "systemctl is-system-running --wait" {
		c.readinessDeadline, _ = ctx.Deadline()
		if c.systemdAttempted != nil {
			select {
			case c.systemdAttempted <- struct{}{}:
			default:
			}
		}
		if err := ctx.Err(); err != nil {
			return "", "", err
		}
		if len(c.systemdResponses) > 0 {
			response := c.systemdResponses[0]
			if len(c.systemdResponses) > 1 {
				c.systemdResponses = c.systemdResponses[1:]
			}
			return response.stdout, response.stderr, response.err
		}
		state := c.systemdState
		if state == "" {
			state = "running\n"
		}
		return state, "", c.systemdErr
	}
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	return "", "", nil
}

func assertCalls(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%v\nwant:\n%v", got, want)
	}
}

func assertOrderedCalls(t *testing.T, calls, want []string) {
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

func countCall(calls []string, want string) int {
	count := 0
	for _, call := range calls {
		if call == want {
			count++
		}
	}
	return count
}

func containsCall(calls []string, want string) bool {
	for _, call := range calls {
		if call == want {
			return true
		}
	}
	return false
}

func equalStringMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if b[key] != value {
			return false
		}
	}
	return true
}
