package provision

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/lxc/incus/v7/shared/api"
	"github.com/sklarsa/kanedias/internal/config"
	"github.com/sklarsa/kanedias/internal/incusclient"
	"github.com/sklarsa/kanedias/internal/supervisor/contract"
)

var expectedChildProvisionSteps = []string{
	"check configured proxy listener",
	"resolve workspace pool",
	"resolve parent effective root pool",
	"require the same created Btrfs pool for root and volume",
	"verify parent instance and volume",
	"copy child workspace volume",
	"copy stopped child instance",
	"replace workspace device",
	"replace supervisor proxy device",
	"write child instance metadata",
	"write child volume metadata",
	"verify local devices",
	"start child instance",
	"wait for RPC address",
}

type recordingChildClient struct {
	poolName     string
	pool         *api.StoragePool
	parent       *api.Instance
	child        *api.Instance
	parentVolume *api.StorageVolume
	childVolume  *api.StorageVolume

	copyVolumeErr   error
	copyInstanceErr error
	updateErr       error
	startErr        error
	startErrs       []error
	calls           []string
	instancePut     *api.InstancePut
	volumePut       *api.StorageVolumePut
}

func newRecordingChildClient() *recordingChildClient {
	return &recordingChildClient{
		poolName: "default",
		pool:     &api.StoragePool{Name: "default", Driver: "btrfs", Status: api.StoragePoolStatusCreated},
		parent: &api.Instance{
			Name: "session-parent",
			InstancePut: api.InstancePut{
				Config: api.ConfigMap{
					"security.nesting":                             "true",
					"environment.KANEDIAS_SESSION_ID":              "parent",
					"environment.KANEDIAS_PI_SESSION_FILE":         "/parent/session.jsonl",
					"environment.KANEDIAS_PARENT_SESSION_ID":       "parent",
					"environment.KANEDIAS_UNAPPROVED_LAUNCH_STATE": "must-not-survive",
				},
				Devices: api.DevicesMap{
					"workspace":  {"type": "disk", "pool": "default", "source": "workspace-parent", "path": "/workspace", "readonly": "true"},
					"supervisor": {"type": "proxy", "connect": "unix:/run/parent.sock", "listen": "unix:/run/kanedias/supervisor.sock", "security.uid": "1000"},
					"extra":      {"type": "none"},
				},
			},
			ExpandedDevices: api.DevicesMap{"root": {"type": "disk", "pool": "default", "path": "/"}},
		},
		parentVolume: &api.StorageVolume{Name: "workspace-parent", StorageVolumePut: api.StorageVolumePut{Config: api.ConfigMap{"size": "5GiB"}}},
	}
}

func (f *recordingChildClient) ResolvePool(context.Context, string) (string, error) {
	f.calls = append(f.calls, "resolve pool")
	return f.poolName, nil
}
func (f *recordingChildClient) GetStoragePool(context.Context, string) (*api.StoragePool, error) {
	f.calls = append(f.calls, "get pool")
	return f.pool, nil
}
func (f *recordingChildClient) GetInstance(_ context.Context, name string) (*api.Instance, string, error) {
	f.calls = append(f.calls, "get instance "+name)
	if name == "session-parent" {
		return f.parent, "parent-etag", nil
	}
	if f.child == nil {
		return nil, "", api.StatusErrorf(404, "missing")
	}
	return f.child, "child-etag", nil
}
func (f *recordingChildClient) GetStorageVolumeWithETag(_ context.Context, _ string, name string) (*api.StorageVolume, string, error) {
	f.calls = append(f.calls, "get volume "+name)
	if name == "workspace-parent" {
		return f.parentVolume, "parent-volume-etag", nil
	}
	if f.childVolume == nil {
		return nil, "", api.StatusErrorf(404, "missing")
	}
	return f.childVolume, "child-volume-etag", nil
}
func (f *recordingChildClient) CopyStorageVolume(_ context.Context, _, source, target string) error {
	f.calls = append(f.calls, "copy volume "+source+" "+target)
	if f.copyVolumeErr != nil {
		return f.copyVolumeErr
	}
	f.childVolume = &api.StorageVolume{Name: target, StorageVolumePut: f.parentVolume.Writable()}
	return nil
}
func (f *recordingChildClient) CopyInstance(_ context.Context, source, target string) error {
	f.calls = append(f.calls, "copy instance "+source+" "+target)
	if f.copyInstanceErr != nil {
		return f.copyInstanceErr
	}
	put := cloneInstancePut(f.parent.Writable())
	f.child = &api.Instance{Name: target, InstancePut: put, Status: "Stopped", StatusCode: api.Stopped}
	return nil
}
func (f *recordingChildClient) UpdateInstance(_ context.Context, name string, put api.InstancePut, etag string) error {
	f.calls = append(f.calls, "update instance "+name+" "+etag)
	f.instancePut = &put
	f.child.InstancePut = put
	return f.updateErr
}
func (f *recordingChildClient) UpdateStorageVolume(_ context.Context, _, name string, put api.StorageVolumePut, etag string) error {
	f.calls = append(f.calls, "update volume "+name+" "+etag)
	f.volumePut = &put
	f.childVolume.StorageVolumePut = put
	return nil
}
func (f *recordingChildClient) StartInstance(_ context.Context, name string) error {
	f.calls = append(f.calls, "start "+name)
	err := f.startErr
	if len(f.startErrs) > 0 {
		err = f.startErrs[0]
		f.startErrs = f.startErrs[1:]
	}
	if err == nil {
		f.child.Status = "Running"
		f.child.StatusCode = api.Running
	}
	return err
}
func (f *recordingChildClient) StopInstance(_ context.Context, name string, force bool) error {
	f.calls = append(f.calls, fmt.Sprintf("stop %s force=%v", name, force))
	f.child.Status = "Stopped"
	f.child.StatusCode = api.Stopped
	return nil
}
func (f *recordingChildClient) DeleteInstance(_ context.Context, name string) error {
	f.calls = append(f.calls, "delete instance "+name)
	f.child = nil
	return nil
}
func (f *recordingChildClient) DeleteStorageVolume(_ context.Context, _, name string) error {
	f.calls = append(f.calls, "delete volume "+name)
	f.childVolume = nil
	return nil
}

func cloneInstancePut(put api.InstancePut) api.InstancePut {
	put.Config = cloneStringMap(put.Config)
	devices := make(api.DevicesMap, len(put.Devices))
	for name, device := range put.Devices {
		devices[name] = cloneStringMap(device)
	}
	put.Devices = devices
	return put
}

func cloneStringMap[M ~map[string]string](source M) M {
	result := make(M, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func validChildRequest() ChildRequest {
	return ChildRequest{
		SessionID: "child-1", ParentID: "parent", RootID: "root",
		SourceInstance: "session-parent", SourceVolume: "workspace-parent",
		HostSocketPath: "/run/kanedias/child-1.sock",
		Worker:         config.WorkerProfile{Provider: "anthropic", Model: "claude-sonnet-4", ThinkingLevel: "high"},
		Contract:       contract.CreateChildRequest{WorkerType: "reviewer", Kind: contract.ChildKindRead, Context: contract.ContextFresh, Task: "Review."},
	}
}

func newTestChildProvisioner(t *testing.T, client *recordingChildClient) *IncusChildProvisioner {
	t.Helper()
	provisioner, err := NewIncusChildProvisioner(client, ChildProvisionOptions{
		WorkspacePool: "default",
		CheckProxy:    func(context.Context) error { return nil },
		WaitRPC: func(_ context.Context, instance string) (string, error) {
			client.calls = append(client.calls, "wait rpc "+instance)
			return "10.0.0.2:4444", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return provisioner
}

func TestNewIncusChildProvisionerRequiresCompleteOptions(t *testing.T) {
	client := newRecordingChildClient()
	valid := ChildProvisionOptions{WorkspacePool: "default", CheckProxy: func(context.Context) error { return nil }, WaitRPC: func(context.Context, string) (string, error) { return "", nil }}
	for _, mutate := range []func(*ChildProvisionOptions){
		func(options *ChildProvisionOptions) { options.WorkspacePool = "" },
		func(options *ChildProvisionOptions) { options.CheckProxy = nil },
		func(options *ChildProvisionOptions) { options.WaitRPC = nil },
	} {
		options := valid
		mutate(&options)
		if _, err := NewIncusChildProvisioner(client, options); err == nil {
			t.Fatalf("NewIncusChildProvisioner(%#v) error = nil, want invalid options", options)
		}
	}
}

func TestProvisionChildFollowsFailClosedOrderAndReplacesInheritedState(t *testing.T) {
	client := newRecordingChildClient()
	provisioner := newTestChildProvisioner(t, client)
	var steps []string
	provisioner.afterStep = func(step string) error { steps = append(steps, step); return nil }

	resources, err := provisioner.ProvisionChild(context.Background(), validChildRequest())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(steps, expectedChildProvisionSteps) {
		t.Fatalf("provision steps = %#v, want %#v", steps, expectedChildProvisionSteps)
	}
	wantResources := &Resources{SessionID: "child-1", Pool: "default", Instance: "session-child-1", Volume: "workspace-child-1", RPCAddr: "10.0.0.2:4444"}
	if !reflect.DeepEqual(resources, wantResources) {
		t.Fatalf("resources = %#v, want %#v", resources, wantResources)
	}

	workspace := map[string]string{"type": "disk", "pool": "default", "source": "workspace-child-1", "path": "/workspace"}
	supervisor := map[string]string{"type": "proxy", "bind": "instance", "listen": "unix:/run/kanedias/supervisor.sock", "connect": "unix:/run/kanedias/child-1.sock", "uid": "1000", "gid": "1000", "mode": "0600"}
	if !reflect.DeepEqual(map[string]string(client.instancePut.Devices["workspace"]), workspace) {
		t.Fatalf("workspace device = %#v, want replacement %#v", client.instancePut.Devices["workspace"], workspace)
	}
	if !reflect.DeepEqual(map[string]string(client.instancePut.Devices["supervisor"]), supervisor) {
		t.Fatalf("supervisor device = %#v, want replacement %#v", client.instancePut.Devices["supervisor"], supervisor)
	}
	if _, ok := client.instancePut.Devices["extra"]; !ok {
		t.Fatalf("unrelated local device was removed: %#v", client.instancePut.Devices)
	}

	wantConfig := map[string]string{
		"user.kanedias.session_id": "child-1", "user.kanedias.parent_session_id": "parent",
		"user.kanedias.root_session_id": "root", "user.kanedias.kind": "read",
		"user.kanedias.context": "fresh", "user.kanedias.worker_type": "reviewer",
		"user.kanedias.workspace_volume":  "workspace-child-1",
		"environment.KANEDIAS_SESSION_ID": "child-1", "environment.KANEDIAS_SESSION_KIND": "read",
		"environment.KANEDIAS_WORKER_TYPE": "reviewer", "environment.KANEDIAS_PI_PROVIDER": "anthropic",
		"environment.KANEDIAS_PI_MODEL": "claude-sonnet-4", "environment.KANEDIAS_PI_THINKING": "high",
		"environment.KANEDIAS_SUPERVISOR_SOCKET": "/run/kanedias/supervisor.sock",
	}
	for key, want := range wantConfig {
		if got := client.instancePut.Config[key]; got != want {
			t.Errorf("instance config %q = %q, want %q", key, got, want)
		}
	}
	wantEnvironment := map[string]string{
		"environment.KANEDIAS_SESSION_ID": "child-1", "environment.KANEDIAS_SESSION_KIND": "read",
		"environment.KANEDIAS_WORKER_TYPE": "reviewer", "environment.KANEDIAS_PI_PROVIDER": "anthropic",
		"environment.KANEDIAS_PI_MODEL": "claude-sonnet-4", "environment.KANEDIAS_PI_THINKING": "high",
		"environment.KANEDIAS_PI_SESSION_FILE": "", "environment.KANEDIAS_SUPERVISOR_SOCKET": "/run/kanedias/supervisor.sock",
	}
	if got := kanediasEnvironment(client.instancePut.Config); !reflect.DeepEqual(got, wantEnvironment) {
		t.Errorf("fresh Kanedias environment = %#v, want exact allowlist %#v", got, wantEnvironment)
	}
	if got := client.instancePut.Config["security.nesting"]; got != "true" {
		t.Errorf("unrelated instance config = %q, want preserved", got)
	}
	for _, key := range []string{"user.kanedias.session_id", "user.kanedias.parent_session_id", "user.kanedias.root_session_id", "user.kanedias.kind", "user.kanedias.context", "user.kanedias.worker_type", "user.kanedias.workspace_volume"} {
		if got, want := client.volumePut.Config[key], wantConfig[key]; got != want {
			t.Errorf("volume metadata %q = %q, want %q", key, got, want)
		}
	}
}

func TestProvisionChildRegeneratesCollidingIncusMACAndRetriesStart(t *testing.T) {
	client := newRecordingChildClient()
	client.parent.Config["volatile.eth0.hwaddr"] = "10:66:6a:cb:6a:3c"
	collision := errors.New(`await submitted Incus local operation: Failed start validation for device "eth0": MAC address "10:66:6a:cb:6a:3c" already defined on another NIC`)
	client.startErrs = []error{collision, nil}

	resources, err := newTestChildProvisioner(t, client).ProvisionChild(context.Background(), validChildRequest())
	if err != nil {
		t.Fatal(err)
	}
	if resources.Instance != "session-child-1" {
		t.Fatalf("instance = %q, want session-child-1", resources.Instance)
	}
	var starts int
	for _, call := range client.calls {
		if call == "start session-child-1" {
			starts++
		}
	}
	if starts != 2 {
		t.Fatalf("start calls = %d, want collision plus one retry: %v", starts, client.calls)
	}
	if _, retained := client.child.Config["volatile.eth0.hwaddr"]; retained {
		t.Fatalf("retried child retained colliding volatile MAC: %#v", client.child.Config)
	}
}

func TestProvisionChildSetsExactForkEnvironmentAllowlist(t *testing.T) {
	client := newRecordingChildClient()
	request := validChildRequest()
	request.Contract.Context = contract.ContextFork
	request.Contract.Fork = &contract.ForkSpec{SessionFile: "/workspace/.pi/child.jsonl", PiSessionID: "pi-child", LeafEntryID: "leaf"}
	if _, err := newTestChildProvisioner(t, client).ProvisionChild(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"environment.KANEDIAS_SESSION_ID": "child-1", "environment.KANEDIAS_SESSION_KIND": "read",
		"environment.KANEDIAS_WORKER_TYPE": "reviewer", "environment.KANEDIAS_PI_PROVIDER": "anthropic",
		"environment.KANEDIAS_PI_MODEL": "claude-sonnet-4", "environment.KANEDIAS_PI_THINKING": "high",
		"environment.KANEDIAS_SUPERVISOR_SOCKET": "/run/kanedias/supervisor.sock",
		"environment.KANEDIAS_PI_SESSION_FILE":   "/workspace/.pi/child.jsonl",
	}
	if got := kanediasEnvironment(client.instancePut.Config); !reflect.DeepEqual(got, want) {
		t.Fatalf("fork Kanedias environment = %#v, want exact allowlist %#v", got, want)
	}
}

func kanediasEnvironment(config api.ConfigMap) map[string]string {
	result := make(map[string]string)
	for key, value := range config {
		if strings.HasPrefix(key, "environment.KANEDIAS_") {
			result[key] = value
		}
	}
	return result
}

func TestProvisionChildRejectsCrossPoolAndUnsupportedStorageBeforeCopies(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*recordingChildClient)
	}{
		{name: "cross pool", mutate: func(client *recordingChildClient) { client.parent.ExpandedDevices["root"]["pool"] = "other" }},
		{name: "pool lookup identity mismatch", mutate: func(client *recordingChildClient) { client.pool.Name = "other" }},
		{name: "unsupported root and volume pool", mutate: func(client *recordingChildClient) { client.pool.Driver = "zfs" }},
		{name: "pool not ready", mutate: func(client *recordingChildClient) { client.pool.Status = api.StoragePoolStatusPending }},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := newRecordingChildClient()
			test.mutate(client)
			_, err := newTestChildProvisioner(t, client).ProvisionChild(context.Background(), validChildRequest())
			if err == nil {
				t.Fatal("ProvisionChild() error = nil, want fail-closed pool error")
			}
			for _, call := range client.calls {
				if strings.HasPrefix(call, "copy ") {
					t.Fatalf("resource copy submitted after pool rejection: %v", client.calls)
				}
			}
		})
	}
}

func TestProvisionChildProxyFailureCreatesNoResources(t *testing.T) {
	client := newRecordingChildClient()
	provisioner, err := NewIncusChildProvisioner(client, ChildProvisionOptions{
		WorkspacePool: "default",
		CheckProxy:    func(context.Context) error { return errors.New("connection refused") },
		WaitRPC:       func(context.Context, string) (string, error) { return "", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = provisioner.ProvisionChild(context.Background(), validChildRequest())
	var contractErr *contract.Error
	if !errors.As(err, &contractErr) || contractErr.Code != contract.ErrorProxyUnavailable {
		t.Fatalf("ProvisionChild() error = %v, want proxy_unavailable", err)
	}
	if client.child != nil || client.childVolume != nil {
		t.Fatalf("proxy failure created resources: instance=%#v volume=%#v", client.child, client.childVolume)
	}
	for _, call := range client.calls {
		if strings.HasPrefix(call, "copy ") {
			t.Fatalf("proxy failure submitted copy: %v", client.calls)
		}
	}
}

func TestProvisionChildFailureAfterEveryStepCleansReverseOwnedOrder(t *testing.T) {
	for failureIndex, failureStep := range expectedChildProvisionSteps {
		t.Run(failureStep, func(t *testing.T) {
			client := newRecordingChildClient()
			provisioner := newTestChildProvisioner(t, client)
			injected := errors.New("injected after " + failureStep)
			stepIndex := 0
			provisioner.afterStep = func(string) error {
				current := stepIndex
				stepIndex++
				if current == failureIndex {
					return injected
				}
				return nil
			}

			if _, err := provisioner.ProvisionChild(context.Background(), validChildRequest()); !errors.Is(err, injected) {
				t.Fatalf("ProvisionChild() error = %v, want injected failure", err)
			}
			var deletes []string
			for _, call := range client.calls {
				if strings.HasPrefix(call, "delete ") {
					deletes = append(deletes, call)
				}
			}
			switch {
			case failureIndex < 5:
				if len(deletes) != 0 {
					t.Fatalf("deletes before ownership = %v", deletes)
				}
			case failureIndex == 5:
				if want := []string{"delete volume workspace-child-1"}; !reflect.DeepEqual(deletes, want) {
					t.Fatalf("deletes = %v, want %v", deletes, want)
				}
			default:
				want := []string{"delete instance session-child-1", "delete volume workspace-child-1"}
				if !reflect.DeepEqual(deletes, want) {
					t.Fatalf("deletes = %v, want %v", deletes, want)
				}
				if failureIndex >= 12 {
					calls := strings.Join(client.calls, "\n")
					stop := strings.Index(calls, "stop session-child-1 force=true")
					deleteInstance := strings.Index(calls, "delete instance session-child-1")
					if stop < 0 || stop > deleteInstance {
						t.Fatalf("running child was not force-stopped before deletion:\n%s", calls)
					}
				}
			}
		})
	}
}

func TestProvisionChildAmbiguousCopyProbesMetadataBeforeCleanup(t *testing.T) {
	for _, resource := range []string{"volume", "instance"} {
		t.Run(resource, func(t *testing.T) {
			client := newRecordingChildClient()
			ambiguous := errors.New("submitted but wait failed")
			if resource == "volume" {
				client.copyVolumeErr = ambiguous
				client.childVolume = &api.StorageVolume{Name: "workspace-child-1", StorageVolumePut: api.StorageVolumePut{Config: api.ConfigMap{"user.kanedias.session_id": "child-1"}}}
			} else {
				client.copyInstanceErr = ambiguous
				client.child = &api.Instance{Name: "session-child-1", InstancePut: api.InstancePut{Config: api.ConfigMap{"user.kanedias.session_id": "child-1"}}}
			}
			provisioner := newTestChildProvisioner(t, client)
			provisioner.operationWasSubmitted = func(err error) bool { return errors.Is(err, ambiguous) }

			if _, err := provisioner.ProvisionChild(context.Background(), validChildRequest()); !errors.Is(err, ambiguous) {
				t.Fatalf("ProvisionChild() error = %v, want ambiguous wait error", err)
			}
			calls := strings.Join(client.calls, "\n")
			if resource == "volume" {
				if !strings.Contains(calls, "get volume workspace-child-1\ndelete volume workspace-child-1") {
					t.Fatalf("ambiguous volume cleanup did not probe then delete:\n%s", calls)
				}
			} else {
				instanceProbe := strings.Index(calls, "get instance session-child-1")
				instanceDelete := strings.Index(calls, "delete instance session-child-1")
				volumeDelete := strings.Index(calls, "delete volume workspace-child-1")
				if instanceProbe < 0 || instanceDelete < instanceProbe || volumeDelete < instanceDelete {
					t.Fatalf("ambiguous instance cleanup order is unsafe:\n%s", calls)
				}
			}
		})
	}
}

func TestProvisionChildAwaitsAmbiguousCopyBeforeFinalProbeAndDelete(t *testing.T) {
	for _, resource := range []string{"volume", "instance"} {
		t.Run(resource, func(t *testing.T) {
			client := newRecordingChildClient()
			ambiguous := errors.New("submitted copy request cancelled")
			if resource == "volume" {
				client.copyVolumeErr = ambiguous
			} else {
				client.copyInstanceErr = ambiguous
			}
			provisioner := newTestChildProvisioner(t, client)
			provisioner.operationWasSubmitted = func(err error) bool { return errors.Is(err, ambiguous) }
			provisioner.awaitSubmittedOperation = func(_ context.Context, err error) error {
				if !errors.Is(err, ambiguous) {
					t.Fatalf("await error = %v, want ambiguous copy error", err)
				}
				if resource == "volume" {
					if _, _, probeErr := client.GetStorageVolumeWithETag(context.Background(), "default", "workspace-child-1"); !incusclient.IsNotFound(probeErr) {
						t.Fatalf("initial volume probe error = %v, want not found", probeErr)
					}
					client.childVolume = &api.StorageVolume{Name: "workspace-child-1"}
				} else {
					if _, _, probeErr := client.GetInstance(context.Background(), "session-child-1"); !incusclient.IsNotFound(probeErr) {
						t.Fatalf("initial instance probe error = %v, want not found", probeErr)
					}
					client.child = &api.Instance{Name: "session-child-1", Status: "Stopped", StatusCode: api.Stopped}
				}
				return nil
			}

			if _, err := provisioner.ProvisionChild(context.Background(), validChildRequest()); !errors.Is(err, ambiguous) {
				t.Fatalf("ProvisionChild() error = %v, want ambiguous copy error", err)
			}
			calls := strings.Join(client.calls, "\n")
			if resource == "volume" {
				if !strings.Contains(calls, "get volume workspace-child-1\nget volume workspace-child-1\ndelete volume workspace-child-1") {
					t.Fatalf("late volume was not re-probed and deleted after terminal wait:\n%s", calls)
				}
			} else if !strings.Contains(calls, "get instance session-child-1\nget instance session-child-1\ndelete instance session-child-1") {
				t.Fatalf("late instance was not re-probed and deleted after terminal wait:\n%s", calls)
			}
		})
	}
}

func TestDestroyDeletesInstanceThenVolume(t *testing.T) {
	client := newRecordingChildClient()
	client.child = &api.Instance{Name: "session-child-1"}
	client.childVolume = &api.StorageVolume{Name: "workspace-child-1"}
	provisioner := newTestChildProvisioner(t, client)
	if err := provisioner.Destroy(context.Background(), &Resources{Pool: "default", Instance: "session-child-1", Volume: "workspace-child-1"}); err != nil {
		t.Fatal(err)
	}
	var deletes []string
	for _, call := range client.calls {
		if strings.HasPrefix(call, "delete ") {
			deletes = append(deletes, call)
		}
	}
	want := []string{"delete instance session-child-1", "delete volume workspace-child-1"}
	if !reflect.DeepEqual(deletes, want) {
		t.Fatalf("deletes = %v, want %v", deletes, want)
	}
}

func TestProvisionChildRejectsInvalidDerivedIncusNames(t *testing.T) {
	client := newRecordingChildClient()
	request := validChildRequest()
	request.SessionID = "bad/name"
	if _, err := newTestChildProvisioner(t, client).ProvisionChild(context.Background(), request); err == nil {
		t.Fatal("ProvisionChild() error = nil, want invalid Incus name")
	}
	if len(client.calls) != 0 {
		t.Fatalf("invalid name performed remote calls: %v", client.calls)
	}
}

func TestProvisionChildAwaitsSubmittedLocalUpdateAndStartBeforeCleanup(t *testing.T) {
	for _, operation := range []string{"update", "start"} {
		t.Run(operation, func(t *testing.T) {
			client := newRecordingChildClient()
			ambiguous := errors.New("submitted local operation wait cancelled")
			if operation == "update" {
				client.updateErr = ambiguous
			} else {
				client.startErr = ambiguous
			}
			provisioner := newTestChildProvisioner(t, client)
			provisioner.operationWasSubmitted = func(err error) bool { return errors.Is(err, ambiguous) }
			awaited := false
			provisioner.awaitSubmittedOperation = func(_ context.Context, err error) error {
				if !errors.Is(err, ambiguous) {
					t.Fatalf("await error = %v", err)
				}
				for _, call := range client.calls {
					if strings.HasPrefix(call, "delete ") || strings.HasPrefix(call, "stop ") {
						t.Fatalf("cleanup started before submitted %s became terminal: %#v", operation, client.calls)
					}
				}
				awaited = true
				return nil
			}
			if _, err := provisioner.ProvisionChild(context.Background(), validChildRequest()); !errors.Is(err, ambiguous) {
				t.Fatalf("ProvisionChild() error = %v", err)
			}
			if !awaited {
				t.Fatalf("submitted %s operation was not awaited", operation)
			}
			calls := strings.Join(client.calls, "\n")
			if !strings.Contains(calls, "delete instance session-child-1\ndelete volume workspace-child-1") {
				t.Fatalf("cleanup order after %s wait:\n%s", operation, calls)
			}
		})
	}
}
