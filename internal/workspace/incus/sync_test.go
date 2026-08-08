package incusworkspace

import (
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

type syncExecResult struct {
	stdout string
	stderr string
	err    error
}

type syncFakeClient struct {
	calls          []string
	pool           *api.StoragePool
	volume         *api.StorageVolume
	createRequest  api.InstancesPost
	errors         map[string]error
	execResults    map[string]syncExecResult
	execSequences  map[string][]syncExecResult
	cancelOnImage  context.CancelFunc
	cleanupCtxSeen bool
	forcedStops    []bool
	updated        api.InstancePut
}

func newSyncFake() *syncFakeClient {
	return &syncFakeClient{
		pool:          &api.StoragePool{Name: "pool1", Driver: "btrfs"},
		errors:        map[string]error{},
		execResults:   map[string]syncExecResult{},
		execSequences: map[string][]syncExecResult{},
	}
}

func (f *syncFakeClient) record(call string)       { f.calls = append(f.calls, call) }
func (f *syncFakeClient) result(call string) error { return f.errors[call] }
func (f *syncFakeClient) observeCleanup(ctx context.Context) {
	deadline, bounded := ctx.Deadline()
	if ctx.Err() == nil && bounded && time.Until(deadline) > 0 && time.Until(deadline) <= cleanupTimeout {
		f.cleanupCtxSeen = true
	}
}
func (f *syncFakeClient) Disconnect() { f.record("disconnect") }
func (f *syncFakeClient) ResolvePool(context.Context, string) (string, error) {
	f.record("resolve-pool")
	return "pool1", f.result("resolve-pool")
}
func (f *syncFakeClient) GetStoragePool(_ context.Context, name string) (*api.StoragePool, error) {
	f.record("get-pool " + name)
	return f.pool, f.result("get-pool")
}
func (f *syncFakeClient) GetStorageVolume(_ context.Context, _, name string) (*api.StorageVolume, error) {
	f.record("get-volume " + name)
	if err := f.result("get-volume"); err != nil {
		return nil, err
	}
	if f.volume == nil {
		return nil, api.StatusErrorf(http.StatusNotFound, "missing")
	}
	return f.volume, nil
}
func (f *syncFakeClient) CreateStorageVolume(_ context.Context, _, name string) error {
	f.record("create-volume " + name)
	return f.result("create-volume")
}
func (f *syncFakeClient) DeleteStorageVolume(ctx context.Context, _, name string) error {
	f.observeCleanup(ctx)
	f.record("delete-volume " + name)
	return f.result("delete-volume")
}
func (f *syncFakeClient) CopyStorageVolume(context.Context, string, string, string) error {
	return errors.New("unexpected CopyStorageVolume call")
}
func (f *syncFakeClient) CopyStorageVolumeUntilTerminal(context.Context, string, string, string) error {
	return errors.New("unexpected CopyStorageVolumeUntilTerminal call")
}
func (f *syncFakeClient) GetNetwork(context.Context, string) (*api.Network, error) {
	return nil, errors.New("unexpected direct GetNetwork call")
}
func (f *syncFakeClient) CreateNetwork(context.Context, api.NetworksPost) error {
	return errors.New("unexpected direct CreateNetwork call")
}
func (f *syncFakeClient) EnsureProfile(_ context.Context, name string, _ []byte) error {
	f.record("ensure-profile " + name)
	return f.result("ensure-profile")
}
func (f *syncFakeClient) CreateInstance(_ context.Context, request api.InstancesPost) error {
	f.createRequest = request
	f.record("create-instance")
	return f.result("create-instance")
}
func (f *syncFakeClient) StartInstance(context.Context, string) error {
	f.record("start-instance")
	return f.result("start-instance")
}
func (f *syncFakeClient) StopInstance(ctx context.Context, _ string, force bool) error {
	f.observeCleanup(ctx)
	f.forcedStops = append(f.forcedStops, force)
	f.record("stop-instance")
	return f.result("stop-instance")
}
func (f *syncFakeClient) GetInstance(ctx context.Context, _ string) (*api.Instance, string, error) {
	f.observeCleanup(ctx)
	f.record("get-instance")
	if err := f.result("get-instance"); err != nil {
		return nil, "", err
	}
	return &api.Instance{InstancePut: api.InstancePut{Devices: api.DevicesMap{
		maintenanceDevice: Device("pool1", config.DefaultIncusWorkspaceVolume),
	}}}, "etag", nil
}
func (f *syncFakeClient) UpdateInstance(ctx context.Context, _ string, request api.InstancePut, _ string) error {
	f.observeCleanup(ctx)
	f.updated = request
	f.record("update-instance")
	return f.result("update-instance")
}
func (f *syncFakeClient) DeleteInstance(ctx context.Context, _ string) error {
	f.observeCleanup(ctx)
	f.record("delete-instance")
	return f.result("delete-instance")
}
func (f *syncFakeClient) Exec(ctx context.Context, _ string, request incusclient.ExecRequest) (string, string, error) {
	command := strings.Join(request.Command, " ")
	call := "exec " + command
	if strings.HasPrefix(command, "systemctl stop incus.") {
		f.observeCleanup(ctx)
	}
	f.record(call)
	if command == "incus image copy images:debian/13 local: --copy-aliases --auto-update --reuse" && f.cancelOnImage != nil {
		f.cancelOnImage()
		return "", "", context.Canceled
	}
	if sequence := f.execSequences[command]; len(sequence) > 0 {
		result := sequence[0]
		f.execSequences[command] = sequence[1:]
		return result.stdout, result.stderr, result.err
	}
	if result, ok := f.execResults[command]; ok {
		return result.stdout, result.stderr, result.err
	}
	switch command {
	case "systemctl is-system-running":
		return "running\n", "", nil
	case "incus query /1.0/storage-pools?recursion=1":
		return `[]`, "", nil
	case "incus query /1.0/storage-pools/default":
		return `{"driver":"btrfs","config":{"source":"/var/lib/incus/storage-pools/default"}}`, "", nil
	case "systemctl show --property=ActiveState --value incus.socket", "systemctl show --property=ActiveState --value incus.service":
		return "inactive\n", "", nil
	case "systemctl show --property=MainPID --value incus.service":
		return "0\n", "", nil
	}
	return "", "", nil
}

func syncTestConfig() config.Config {
	return config.Config{
		Network:   config.Network{IPv4: "10.42.0.1/24"},
		BaseImage: config.BaseImage{Name: "base", Source: "images", Image: "debian/13"},
		Workspace: config.Workspace{Pool: "pool1", Incus: config.IncusWorkspace{
			Volume: config.DefaultIncusWorkspaceVolume,
			Images: []string{"images:debian/13"},
		}},
	}
}

func syncTestDependencies(fake *syncFakeClient) dependencies {
	return dependencies{
		connect: func(context.Context) (client, error) { return fake, nil },
		initCA: func() error {
			fake.record("init-ca")
			return fake.result("init-ca")
		},
		ensureNetwork: func(context.Context, client, config.Config) error {
			fake.record("ensure-network")
			return fake.result("ensure-network")
		},
		renderProfile:         func(io.Writer, string, config.Config) error { return nil },
		newInstanceName:       func() string { return "workspace-incus-sync-test" },
		operationWasSubmitted: func(err error) bool { return errors.Is(err, errSubmitted) },
		awaitSubmittedOperation: func(context.Context, error) error {
			return nil
		},
	}
}

var errSubmitted = errors.New("operation was submitted")

func TestSyncNewSeedSuccessLifecycle(t *testing.T) {
	fake := newSyncFake()
	err := syncWithDependencies(context.Background(), syncTestConfig(), io.Discard, io.Discard, syncTestDependencies(fake))
	if err != nil {
		t.Fatalf("syncWithDependencies() error = %v", err)
	}
	want := []string{
		"resolve-pool",
		"get-pool pool1",
		"get-volume kanedias-incus-seed",
		"create-volume kanedias-incus-seed",
		"init-ca",
		"ensure-network",
		"ensure-profile sandbox",
		"create-instance",
		"start-instance",
		"exec systemctl is-system-running",
		"exec update-ca-certificates",
		"exec getent ahosts images.linuxcontainers.org",
		"exec incus admin waitready --timeout 60",
		"exec incus query /1.0/storage-pools?recursion=1",
		"exec incus admin init --minimal",
		"exec incus admin waitready --timeout 60",
		"exec incus query /1.0/storage-pools/default",
		"exec incus image copy images:debian/13 local: --copy-aliases --auto-update --reuse",
		"exec systemctl stop incus.socket",
		"exec systemctl stop incus.service",
		"exec systemctl show --property=ActiveState --value incus.socket",
		"exec systemctl show --property=ActiveState --value incus.service",
		"exec systemctl show --property=MainPID --value incus.service",
		"stop-instance",
		"get-instance",
		"update-instance",
		"delete-instance",
		"disconnect",
	}
	if !reflect.DeepEqual(fake.calls, want) {
		t.Fatalf("calls =\n%s\nwant:\n%s", strings.Join(fake.calls, "\n"), strings.Join(want, "\n"))
	}
	if fake.createRequest.Source.Alias != "base" {
		t.Fatalf("source alias = %q, want base", fake.createRequest.Source.Alias)
	}
	if got := fake.createRequest.Devices["root"]; !reflect.DeepEqual(got, map[string]string{"type": "disk", "pool": "pool1", "path": "/"}) {
		t.Fatalf("root device = %#v", got)
	}
	if got := fake.createRequest.Devices[maintenanceDevice]; !reflect.DeepEqual(got, Device("pool1", config.DefaultIncusWorkspaceVolume)) {
		t.Fatalf("maintenance device = %#v", got)
	}
}

func TestSyncRejectsNonBtrfsPoolBeforeSeedLookup(t *testing.T) {
	fake := newSyncFake()
	fake.pool.Driver = "dir"
	err := syncWithDependencies(context.Background(), syncTestConfig(), io.Discard, io.Discard, syncTestDependencies(fake))
	if err == nil || !strings.Contains(err.Error(), `outer Incus storage pool "pool1" uses "dir", want btrfs`) {
		t.Fatalf("error = %v", err)
	}
	if got, want := fake.calls, []string{"resolve-pool", "get-pool pool1", "disconnect"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
}

func TestSyncExistingInitializedSeedSkipsCreationAndInitialization(t *testing.T) {
	fake := newSyncFake()
	fake.volume = &api.StorageVolume{Name: config.DefaultIncusWorkspaceVolume}
	fake.execResults["incus query /1.0/storage-pools?recursion=1"] = syncExecResult{stdout: `[{"name":"default","driver":"btrfs"}]`}
	if err := syncWithDependencies(context.Background(), syncTestConfig(), io.Discard, io.Discard, syncTestDependencies(fake)); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(fake.calls, "\n")
	for _, forbidden := range []string{"create-volume", "exec incus admin init --minimal"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("unexpected call containing %q:\n%s", forbidden, joined)
		}
	}
	for _, required := range []string{"exec incus query /1.0/storage-pools?recursion=1", "exec incus query /1.0/storage-pools/default", "exec incus image copy images:debian/13"} {
		if !strings.Contains(joined, required) {
			t.Fatalf("missing call containing %q", required)
		}
	}
}

func TestSyncExistingUninitializedSeedInitializesThenValidates(t *testing.T) {
	fake := newSyncFake()
	fake.volume = &api.StorageVolume{Name: config.DefaultIncusWorkspaceVolume}
	if err := syncWithDependencies(context.Background(), syncTestConfig(), io.Discard, io.Discard, syncTestDependencies(fake)); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(fake.calls, "\n")
	for _, required := range []string{
		"exec incus query /1.0/storage-pools?recursion=1",
		"exec incus admin init --minimal",
		"exec incus query /1.0/storage-pools/default",
	} {
		if !strings.Contains(joined, required) {
			t.Fatalf("missing call %q:\n%s", required, joined)
		}
	}
	if strings.Contains(joined, "create-volume") {
		t.Fatalf("existing volume was recreated:\n%s", joined)
	}
}

func TestSyncRejectsAttachedExistingSeedBeforeMaintenanceCreation(t *testing.T) {
	fake := newSyncFake()
	fake.volume = &api.StorageVolume{Name: config.DefaultIncusWorkspaceVolume, UsedBy: []string{"/1.0/instances/busy"}}
	err := syncWithDependencies(context.Background(), syncTestConfig(), io.Discard, io.Discard, syncTestDependencies(fake))
	if err == nil || !strings.Contains(err.Error(), "is attached") {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(strings.Join(fake.calls, "\n"), "create-instance") {
		t.Fatalf("maintenance instance created: %v", fake.calls)
	}
}

func TestSyncNewSeedInitializationFailureDeletesSeedAfterInstanceCleanup(t *testing.T) {
	fake := newSyncFake()
	fake.execResults["incus admin init --minimal"] = syncExecResult{err: errors.New("init failed")}
	err := syncWithDependencies(context.Background(), syncTestConfig(), io.Discard, io.Discard, syncTestDependencies(fake))
	if err == nil {
		t.Fatal("expected error")
	}
	assertCallBefore(t, fake.calls, "delete-instance", "delete-volume kanedias-incus-seed")
}

func TestSyncExistingSeedRefreshFailurePreservesSeed(t *testing.T) {
	fake := newSyncFake()
	fake.volume = &api.StorageVolume{Name: config.DefaultIncusWorkspaceVolume}
	fake.execResults["incus image copy images:debian/13 local: --copy-aliases --auto-update --reuse"] = syncExecResult{err: errors.New("copy failed")}
	if err := syncWithDependencies(context.Background(), syncTestConfig(), io.Discard, io.Discard, syncTestDependencies(fake)); err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(strings.Join(fake.calls, "\n"), "delete-volume") {
		t.Fatalf("existing seed deleted: %v", fake.calls)
	}
}

func TestSyncCancellationUsesBoundedNonCancelledCleanupContext(t *testing.T) {
	fake := newSyncFake()
	ctx, cancel := context.WithCancel(context.Background())
	fake.cancelOnImage = cancel
	if err := syncWithDependencies(ctx, syncTestConfig(), io.Discard, io.Discard, syncTestDependencies(fake)); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context canceled", err)
	}
	if !fake.cleanupCtxSeen {
		t.Fatal("cleanup did not observe a live context with a cleanup deadline")
	}
}

func TestWaitForDNSRetriesUntilSuccess(t *testing.T) {
	fake := newSyncFake()
	command := "getent ahosts images.linuxcontainers.org"
	fake.execSequences[command] = []syncExecResult{
		{stderr: "temporary failure", err: errors.New("lookup failed")},
		{},
	}
	if err := waitForDNS(context.Background(), fake, "maintenance", time.Second, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if got := countExactCall(fake.calls, "exec "+command); got != 2 {
		t.Fatalf("DNS attempts = %d, want 2", got)
	}
}

func TestWaitForDNSStopsAtTimeout(t *testing.T) {
	fake := newSyncFake()
	command := "getent ahosts images.linuxcontainers.org"
	fake.execResults[command] = syncExecResult{stderr: "temporary failure", err: errors.New("lookup failed")}
	err := waitForDNS(context.Background(), fake, "maintenance", 10*time.Millisecond, time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waitForDNS error = %v, want deadline exceeded", err)
	}
	if got := countExactCall(fake.calls, "exec "+command); got < 2 {
		t.Fatalf("DNS attempts = %d, want retries", got)
	}
}

func TestWaitForDNSStopsOnCallerCancellation(t *testing.T) {
	fake := newSyncFake()
	command := "getent ahosts images.linuxcontainers.org"
	fake.execResults[command] = syncExecResult{stderr: "temporary failure", err: errors.New("lookup failed")}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := waitForDNS(ctx, fake, "maintenance", time.Minute, time.Millisecond)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("waitForDNS error = %v, want context canceled", err)
	}
}

func TestSyncCleanupQuiescesBeforeForcedOuterStop(t *testing.T) {
	fake := newSyncFake()
	fake.execResults["incus image copy images:debian/13 local: --copy-aliases --auto-update --reuse"] = syncExecResult{err: errors.New("copy failed")}
	_ = syncWithDependencies(context.Background(), syncTestConfig(), io.Discard, io.Discard, syncTestDependencies(fake))
	assertCallBefore(t, fake.calls, "exec systemctl stop incus.socket", "stop-instance")
	if len(fake.forcedStops) != 1 || !fake.forcedStops[0] {
		t.Fatalf("forced stops = %v, want [true]", fake.forcedStops)
	}
}

func TestSyncQuiesceFailureIsReturned(t *testing.T) {
	fake := newSyncFake()
	quiesceErr := errors.New("cannot stop nested Incus")
	fake.execResults["systemctl stop incus.socket"] = syncExecResult{err: quiesceErr}
	err := syncWithDependencies(context.Background(), syncTestConfig(), io.Discard, io.Discard, syncTestDependencies(fake))
	if !errors.Is(err, quiesceErr) {
		t.Fatalf("error = %v, want quiesce error", err)
	}
}

func TestSyncSubmittedCreateErrorCleansUpPotentialInstance(t *testing.T) {
	fake := newSyncFake()
	fake.errors["create-instance"] = errSubmitted
	deps := syncTestDependencies(fake)
	deps.awaitSubmittedOperation = func(ctx context.Context, err error) error {
		if !errors.Is(err, errSubmitted) {
			t.Fatalf("await error = %v, want submitted error", err)
		}
		fake.observeCleanup(ctx)
		fake.record("await-submitted")
		return nil
	}
	err := syncWithDependencies(context.Background(), syncTestConfig(), io.Discard, io.Discard, deps)
	if !errors.Is(err, errSubmitted) {
		t.Fatalf("error = %v", err)
	}
	for _, call := range []string{"get-instance", "update-instance", "delete-instance"} {
		if !containsCall(fake.calls, call) {
			t.Fatalf("missing cleanup call %q: %v", call, fake.calls)
		}
	}
	assertCallBefore(t, fake.calls, "await-submitted", "get-instance")
	if !fake.cleanupCtxSeen {
		t.Fatal("submitted operation await did not use the bounded cleanup context")
	}
}

func TestSyncSubmittedStartErrorQuiescesAndForceStops(t *testing.T) {
	fake := newSyncFake()
	fake.errors["start-instance"] = errSubmitted
	err := syncWithDependencies(context.Background(), syncTestConfig(), io.Discard, io.Discard, syncTestDependencies(fake))
	if !errors.Is(err, errSubmitted) {
		t.Fatalf("error = %v", err)
	}
	assertCallBefore(t, fake.calls, "exec systemctl stop incus.socket", "stop-instance")
	if len(fake.forcedStops) != 1 || !fake.forcedStops[0] {
		t.Fatalf("forced stops = %v, want [true]", fake.forcedStops)
	}
}

func TestSyncCleanupDetachesSeedBeforeDeletingInstance(t *testing.T) {
	fake := newSyncFake()
	fake.execResults["incus image copy images:debian/13 local: --copy-aliases --auto-update --reuse"] = syncExecResult{err: errors.New("copy failed")}
	_ = syncWithDependencies(context.Background(), syncTestConfig(), io.Discard, io.Discard, syncTestDependencies(fake))
	if _, present := fake.updated.Devices[maintenanceDevice]; present {
		t.Fatalf("maintenance device still present in update: %#v", fake.updated.Devices)
	}
	assertCallBefore(t, fake.calls, "update-instance", "delete-instance")
}

func TestSyncJoinsCleanupErrorWithPrimaryError(t *testing.T) {
	fake := newSyncFake()
	primary := errors.New("copy failed")
	cleanup := errors.New("delete failed")
	fake.execResults["incus image copy images:debian/13 local: --copy-aliases --auto-update --reuse"] = syncExecResult{err: primary}
	fake.errors["delete-instance"] = cleanup
	err := syncWithDependencies(context.Background(), syncTestConfig(), io.Discard, io.Discard, syncTestDependencies(fake))
	if !errors.Is(err, primary) || !errors.Is(err, cleanup) {
		t.Fatalf("error = %v, want both primary and cleanup errors", err)
	}
}

func TestSyncValidatesLifecycleBeforeConnecting(t *testing.T) {
	cfg := syncTestConfig()
	cfg.BaseImage.Name = ""
	connected := false
	deps := dependencies{connect: func(context.Context) (client, error) {
		connected = true
		return nil, fmt.Errorf("unexpected connection")
	}}
	if err := syncWithDependencies(context.Background(), cfg, io.Discard, io.Discard, deps); err == nil {
		t.Fatal("expected validation error")
	}
	if connected {
		t.Fatal("connected before validation")
	}
}

func assertCallBefore(t *testing.T, calls []string, first, second string) {
	t.Helper()
	firstIndex, secondIndex := -1, -1
	for index, call := range calls {
		if call == first && firstIndex == -1 {
			firstIndex = index
		}
		if call == second && secondIndex == -1 {
			secondIndex = index
		}
	}
	if firstIndex == -1 || secondIndex == -1 || firstIndex >= secondIndex {
		t.Fatalf("want %q before %q, calls: %v", first, second, calls)
	}
}

func countExactCall(calls []string, wanted string) int {
	count := 0
	for _, call := range calls {
		if call == wanted {
			count++
		}
	}
	return count
}

func containsCall(calls []string, wanted string) bool {
	for _, call := range calls {
		if call == wanted {
			return true
		}
	}
	return false
}
