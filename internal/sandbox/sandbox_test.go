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
	incusworkspace "github.com/sklarsa/kanedias/internal/workspace/incus"
)

func TestCreateOrdersLifecycleAndBuildsOwnedWorkspaceDevice(t *testing.T) {
	fake := &recordingClient{}
	deps := testDependencies(fake)

	if err := create(context.Background(), testConfig(), "demo", io.Discard, io.Discard, deps); err != nil {
		t.Fatal(err)
	}

	wantCalls := []string{
		"resolve-pool", "lock", "init-ca", "ensure-network", "ensure-profile sandbox",
		"get-volume kanedias-workspace-seed",
		"copy-volume kanedias-workspace-seed kanedias-workspace-demo",
		"clone-incus kanedias-incus-seed kanedias-incus-demo",
		"create-instance", "start",
		"exec systemctl is-system-running --wait",
		"exec update-ca-certificates",
		"exec incus admin waitready --timeout 60",
		"exec incus query /1.0/storage-pools/default",
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
	wantIncusDevice := map[string]string{
		"type": "disk", "pool": "pool1", "source": "kanedias-incus-demo", "path": "/var/lib/incus",
	}
	if got := request.Devices[incusworkspace.DeviceName]; !equalStringMap(got, wantIncusDevice) {
		t.Fatalf("Incus-state device = %#v, want %#v", got, wantIncusDevice)
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

func TestCreateRepositoryCopyFailureDoesNotCloneIncusState(t *testing.T) {
	fake := &recordingClient{copyErr: errors.New("copy failed")}
	err := create(context.Background(), testConfig(), "demo", io.Discard, io.Discard, testDependencies(fake))
	if err == nil {
		t.Fatal("Create succeeded after repository copy failure")
	}
	if containsCallPrefix(fake.calls, "clone-incus ") {
		t.Fatalf("Incus state clone attempted after repository copy failure: %v", fake.calls)
	}
}

func TestCreateSubmittedRepositoryCopyFailureRollsBackOwnedCloneWithoutIncusClone(t *testing.T) {
	submitted := errors.New("submitted repository copy failed")
	fake := &recordingClient{copyErr: submitted, submittedErr: submitted}
	deps := testDependencies(fake)
	deps.awaitSubmittedOperation = func(ctx context.Context, err error) error {
		if !errors.Is(err, submitted) {
			t.Fatalf("await error = %v, want submitted copy error", err)
		}
		deadline, bounded := ctx.Deadline()
		if ctx.Err() != nil || !bounded || time.Until(deadline) <= 0 || time.Until(deadline) > 30*time.Second {
			t.Fatalf("await context is not a live bounded cleanup context: err=%v bounded=%v deadline=%v", ctx.Err(), bounded, deadline)
		}
		fake.calls = append(fake.calls, "await-submitted")
		return nil
	}
	err := create(context.Background(), testConfig(), "demo", io.Discard, io.Discard, deps)
	if !errors.Is(err, submitted) {
		t.Fatalf("Create error = %v, want submitted copy error", err)
	}
	if containsCallPrefix(fake.calls, "clone-incus ") {
		t.Fatalf("Incus state clone attempted after repository copy failure: %v", fake.calls)
	}
	assertCalls(t, fake.calls[len(fake.calls)-2:], []string{
		"await-submitted", "delete-volume kanedias-workspace-demo",
	})
}

func TestCreateIncusCloneFailureCleansAmbiguousIncusAndRepositoryClones(t *testing.T) {
	fake := &recordingClient{
		cloneIncusResult: incusworkspace.CloneResult{Name: "kanedias-incus-demo", Created: true},
		cloneIncusErr:    errors.New("clone wait failed"),
	}
	err := create(context.Background(), testConfig(), "demo", io.Discard, io.Discard, testDependencies(fake))
	if err == nil {
		t.Fatal("Create succeeded after Incus-state clone failure")
	}
	assertCalls(t, fake.calls[len(fake.calls)-2:], []string{
		"delete-volume kanedias-incus-demo", "delete-volume kanedias-workspace-demo",
	})
}

func TestCreatePreSubmissionFailureDeletesBothClonesButNotCollidingInstance(t *testing.T) {
	fake := &recordingClient{createErr: errors.New("name collision")}
	err := create(context.Background(), testConfig(), "demo", io.Discard, io.Discard, testDependencies(fake))
	if err == nil {
		t.Fatal("Create succeeded after instance collision")
	}
	if containsCall(fake.calls, "delete-instance") {
		t.Fatalf("calls include deletion of unowned instance: %v", fake.calls)
	}
	assertCalls(t, fake.calls[len(fake.calls)-2:], []string{
		"delete-volume kanedias-incus-demo", "delete-volume kanedias-workspace-demo",
	})
}

func TestCreateSubmittedCreationFailureDeletesDeterministicInstanceAndBothClones(t *testing.T) {
	submitted := errors.New("submitted create failed")
	fake := &recordingClient{createErr: submitted, submittedErr: submitted}
	err := create(context.Background(), testConfig(), "demo", io.Discard, io.Discard, testDependencies(fake))
	if err == nil {
		t.Fatal("Create succeeded after submitted creation failure")
	}
	assertCalls(t, fake.calls[len(fake.calls)-3:], []string{
		"delete-instance", "delete-volume kanedias-incus-demo", "delete-volume kanedias-workspace-demo",
	})
}

func TestCreateSubmittedStartFailureForceStopsBeforeDeletingOwnedResources(t *testing.T) {
	submitted := errors.New("submitted start failed")
	fake := &recordingClient{startErr: submitted, submittedErr: submitted}
	err := create(context.Background(), testConfig(), "demo", io.Discard, io.Discard, testDependencies(fake))
	if err == nil {
		t.Fatal("Create succeeded after submitted start failure")
	}
	assertCalls(t, fake.calls[len(fake.calls)-4:], []string{
		"stop", "delete-instance", "delete-volume kanedias-incus-demo", "delete-volume kanedias-workspace-demo",
	})
}

func TestCreateNestedReadinessFailureCleansInstanceThenBothClones(t *testing.T) {
	fake := &recordingClient{nestedWaitErr: errors.New("nested Incus unavailable")}
	err := create(context.Background(), testConfig(), "demo", io.Discard, io.Discard, testDependencies(fake))
	if err == nil {
		t.Fatal("Create succeeded after nested Incus readiness failure")
	}
	assertCalls(t, fake.calls[len(fake.calls)-4:], []string{
		"stop", "delete-instance", "delete-volume kanedias-incus-demo", "delete-volume kanedias-workspace-demo",
	})
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
	assertCalls(t, fake.calls[len(fake.calls)-4:], []string{
		"stop", "delete-instance", "delete-volume kanedias-incus-demo", "delete-volume kanedias-workspace-demo",
	})
	if len(fake.cleanupContextErrs) != 4 {
		t.Fatalf("cleanup context count = %d, want 4", len(fake.cleanupContextErrs))
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

func TestDestroyStopsRunningVerifiedInstanceBeforeDeletingInstanceAndVolumes(t *testing.T) {
	fake := &recordingClient{
		instance: verifiedInstance(api.Running),
		running:  true,
	}
	if err := destroy(context.Background(), testConfig(), "demo", io.Discard, io.Discard, testDependencies(fake)); err != nil {
		t.Fatal(err)
	}
	assertCalls(t, fake.calls[len(fake.calls)-7:], []string{
		"get-instance", "stop", "delete-instance",
		"get-volume kanedias-incus-demo", "delete-volume kanedias-incus-demo",
		"get-volume kanedias-workspace-demo", "delete-volume kanedias-workspace-demo",
	})
}

func TestDestroyRefusesUnverifiedOwnedDevicesWithoutDeletion(t *testing.T) {
	valid := verifiedInstance(api.Stopped).Devices
	for _, test := range []struct {
		name    string
		devices api.DevicesMap
	}{
		{name: "missing workspace", devices: api.DevicesMap{incusworkspace.DeviceName: valid[incusworkspace.DeviceName]}},
		{name: "mismatched workspace", devices: api.DevicesMap{
			"workspace": {"source": "someone-elses-volume"}, incusworkspace.DeviceName: valid[incusworkspace.DeviceName],
		}},
		{name: "missing Incus state", devices: api.DevicesMap{"workspace": valid["workspace"]}},
		{name: "mismatched Incus state", devices: api.DevicesMap{
			"workspace": valid["workspace"], incusworkspace.DeviceName: {"source": "someone-elses-incus"},
		}},
		{name: "workspace missing pool", devices: api.DevicesMap{
			"workspace": {"type": "disk", "source": "kanedias-workspace-demo", "path": "/workspace"}, incusworkspace.DeviceName: valid[incusworkspace.DeviceName],
		}},
		{name: "workspace wrong pool", devices: api.DevicesMap{
			"workspace": {"type": "disk", "pool": "old-pool", "source": "kanedias-workspace-demo", "path": "/workspace"}, incusworkspace.DeviceName: valid[incusworkspace.DeviceName],
		}},
		{name: "workspace wrong type", devices: api.DevicesMap{
			"workspace": {"type": "unix-block", "pool": "pool1", "source": "kanedias-workspace-demo", "path": "/workspace"}, incusworkspace.DeviceName: valid[incusworkspace.DeviceName],
		}},
		{name: "workspace wrong path", devices: api.DevicesMap{
			"workspace": {"type": "disk", "pool": "pool1", "source": "kanedias-workspace-demo", "path": "/other"}, incusworkspace.DeviceName: valid[incusworkspace.DeviceName],
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &recordingClient{instance: &api.Instance{InstancePut: api.InstancePut{Devices: test.devices}}}
			err := destroy(context.Background(), testConfig(), "demo", io.Discard, io.Discard, testDependencies(fake))
			if err == nil {
				t.Fatal("Destroy succeeded without ownership proof")
			}
			if containsCall(fake.calls, "delete-instance") || containsCallPrefix(fake.calls, "delete-volume ") {
				t.Fatalf("Destroy attempted deletion: %v", fake.calls)
			}
		})
	}
}

func TestDestroyRemovesBothOrphanedCloneVolumes(t *testing.T) {
	missing := api.StatusErrorf(http.StatusNotFound, "missing")
	fake := &recordingClient{getInstanceErr: missing}
	if err := destroy(context.Background(), testConfig(), "demo", io.Discard, io.Discard, testDependencies(fake)); err != nil {
		t.Fatal(err)
	}
	assertOrderedCalls(t, fake.calls, []string{
		"get-instance",
		"get-volume kanedias-incus-demo", "delete-volume kanedias-incus-demo",
		"get-volume kanedias-workspace-demo", "delete-volume kanedias-workspace-demo",
	})
}

func TestDestroyAttemptsBothVolumeDeletionsAndJoinsErrors(t *testing.T) {
	incusErr := errors.New("delete Incus state")
	workspaceErr := errors.New("delete workspace")
	fake := &recordingClient{getInstanceErr: api.StatusErrorf(http.StatusNotFound, "missing"), deleteVolumeErrs: map[string]error{
		"kanedias-incus-demo": incusErr, "kanedias-workspace-demo": workspaceErr,
	}}
	err := destroy(context.Background(), testConfig(), "demo", io.Discard, io.Discard, testDependencies(fake))
	if !errors.Is(err, incusErr) || !errors.Is(err, workspaceErr) {
		t.Fatalf("Destroy error = %v, want both deletion errors", err)
	}
	assertOrderedCalls(t, fake.calls, []string{"delete-volume kanedias-incus-demo", "delete-volume kanedias-workspace-demo"})
}

func TestDestroySucceedsWhenResourcesAreAbsent(t *testing.T) {
	missing := api.StatusErrorf(http.StatusNotFound, "missing")
	fake := &recordingClient{getInstanceErr: missing, getVolumeErr: missing}
	if err := destroy(context.Background(), testConfig(), "demo", io.Discard, io.Discard, testDependencies(fake)); err != nil {
		t.Fatal(err)
	}
	if containsCall(fake.calls, "delete-instance") || containsCallPrefix(fake.calls, "delete-volume ") {
		t.Fatalf("Destroy attempted deletion: %v", fake.calls)
	}
}

func TestDestroyNeverSelectsSeedVolumes(t *testing.T) {
	for _, test := range []struct {
		name    string
		sandbox string
		cfg     config.Config
	}{
		{name: "workspace seed as workspace clone", sandbox: "seed", cfg: func() config.Config {
			cfg := testConfig()
			cfg.Workspace.Incus.Volume = "other-incus-seed"
			return cfg
		}()},
		{name: "Incus seed as Incus clone", sandbox: "seed", cfg: func() config.Config {
			cfg := testConfig()
			cfg.Workspace.Volume = "other-workspace-seed"
			return cfg
		}()},
		{name: "Incus seed as workspace clone", sandbox: "demo", cfg: func() config.Config {
			cfg := testConfig()
			cfg.Workspace.Incus.Volume = "kanedias-workspace-demo"
			return cfg
		}()},
		{name: "workspace seed as Incus clone", sandbox: "demo", cfg: func() config.Config {
			cfg := testConfig()
			cfg.Workspace.Volume = "kanedias-incus-demo"
			return cfg
		}()},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &recordingClient{}
			err := destroy(context.Background(), test.cfg, test.sandbox, io.Discard, io.Discard, testDependencies(fake))
			if err == nil {
				t.Fatal("Destroy accepted a name whose owned volume would be a seed")
			}
			if containsCallPrefix(fake.calls, "delete-volume ") {
				t.Fatalf("Destroy selected seed volume: %v", fake.calls)
			}
		})
	}
}

func verifiedInstance(status api.StatusCode) *api.Instance {
	return &api.Instance{
		StatusCode: status,
		InstancePut: api.InstancePut{Devices: api.DevicesMap{
			"workspace": {
				"type": "disk", "pool": "pool1", "source": "kanedias-workspace-demo", "path": "/workspace",
			},
			incusworkspace.DeviceName: {
				"type": "disk", "pool": "pool1", "source": "kanedias-incus-demo", "path": "/var/lib/incus",
			},
		}},
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
		_ = second.Close()
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
		cloneIncusState: func(_ context.Context, _ incusworkspace.VolumeClient, _, seed, sandbox string) (incusworkspace.CloneResult, error) {
			result := client.cloneIncusResult
			if result.Name == "" {
				result = incusworkspace.CloneResult{Name: incusworkspace.SandboxVolume(sandbox), Created: true}
			}
			client.calls = append(client.calls, "clone-incus "+seed+" "+result.Name)
			return result, client.cloneIncusErr
		},
		waitNestedIncus:       incusworkspace.WaitReady,
		verifyNestedIncus:     incusworkspace.VerifyNativeBtrfs,
		operationWasSubmitted: func(err error) bool { return client.submittedErr != nil && errors.Is(err, client.submittedErr) },
		awaitSubmittedOperation: func(context.Context, error) error {
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
	getInstanceErr       error
	getVolumeErr         error
	copyErr              error
	deleteVolumeErrs     map[string]error
	cloneIncusResult     incusworkspace.CloneResult
	cloneIncusErr        error
	createErr            error
	submittedErr         error
	startErr             error
	startFunc            func(context.Context) error
	nestedWaitErr        error
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
	return &api.StorageVolume{Name: name}, nil
}

func (c *recordingClient) CopyStorageVolume(_ context.Context, _, source, target string) error {
	c.calls = append(c.calls, "copy-volume "+source+" "+target)
	return c.copyErr
}

func (c *recordingClient) CopyStorageVolumeUntilTerminal(_ context.Context, _, source, target string) error {
	c.calls = append(c.calls, "copy-volume-terminal "+source+" "+target)
	return c.copyErr
}

func (c *recordingClient) DeleteStorageVolume(ctx context.Context, _, name string) error {
	c.calls = append(c.calls, "delete-volume "+name)
	c.recordCleanupContext(ctx)
	return c.deleteVolumeErrs[name]
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
	return c.startErr
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
		return "running\n", "", nil
	}
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	switch command {
	case "incus admin waitready --timeout 60":
		return "", "", c.nestedWaitErr
	case "incus query /1.0/storage-pools/default":
		return `{"driver":"btrfs","config":{"source":"/var/lib/incus/storage-pools/default"}}`, "", nil
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

func containsCallPrefix(calls []string, prefix string) bool {
	for _, call := range calls {
		if strings.HasPrefix(call, prefix) {
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
