package provision

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lxc/incus/v7/shared/api"
	"github.com/sklarsa/kanedias/internal/config"
	"github.com/sklarsa/kanedias/internal/incusclient"
	"github.com/sklarsa/kanedias/internal/supervisor/contract"
)

const (
	metaSessionID = "user.kanedias.session_id"
	metaParentID  = "user.kanedias.parent_session_id"
	metaRootID    = "user.kanedias.root_session_id"
	metaKind      = "user.kanedias.kind"
	metaContext   = "user.kanedias.context"
	metaWorker    = "user.kanedias.worker_type"
	metaVolume    = "user.kanedias.workspace_volume"
	metaRun       = "user.kanedias.e2e_run"

	guestSupervisorSocket = "/run/kanedias/supervisor.sock"
	childCleanupTimeout   = 30 * time.Second
)

type childIncusClient interface {
	ResolvePool(context.Context, string) (string, error)
	GetStoragePool(context.Context, string) (*api.StoragePool, error)
	GetInstance(context.Context, string) (*api.Instance, string, error)
	GetStorageVolumeWithETag(context.Context, string, string) (*api.StorageVolume, string, error)
	CopyStorageVolume(context.Context, string, string, string) error
	CopyInstance(context.Context, string, string) error
	UpdateInstance(context.Context, string, api.InstancePut, string) error
	UpdateStorageVolume(context.Context, string, string, api.StorageVolumePut, string) error
	StartInstance(context.Context, string) error
	Exec(context.Context, string, incusclient.ExecRequest) (string, string, error)
	StopInstance(context.Context, string, bool) error
	DeleteInstance(context.Context, string) error
	DeleteStorageVolume(context.Context, string, string) error
}

type ChildProvisionOptions struct {
	WorkspacePool string
	CheckProxy    func(context.Context) error
	WaitRPC       func(context.Context, string) (string, error)
}

type ConfiguredChildProvisioner struct {
	*IncusChildProvisioner
	client *incusclient.Client
}

func NewConfiguredChildProvisioner(ctx context.Context, cfg config.Config) (*ConfiguredChildProvisioner, error) {
	client, err := incusclient.Connect(ctx)
	if err != nil {
		return nil, err
	}
	pool, err := client.ResolvePool(ctx, cfg.Workspace.Pool)
	if err != nil {
		client.Disconnect()
		return nil, err
	}
	provisioner, err := NewIncusChildProvisioner(client, ChildProvisionOptions{
		WorkspacePool: pool,
		CheckProxy: func(ctx context.Context) error {
			return checkRootProxy(ctx, cfg)
		},
		WaitRPC: func(ctx context.Context, instance string) (string, error) {
			return waitForRootRPCAddress(ctx, client, instance, 60*time.Second, 500*time.Millisecond)
		},
	})
	if err != nil {
		client.Disconnect()
		return nil, err
	}
	return &ConfiguredChildProvisioner{IncusChildProvisioner: provisioner, client: client}, nil
}

func (provisioner *ConfiguredChildProvisioner) Close() {
	if provisioner != nil && provisioner.client != nil {
		provisioner.client.Disconnect()
	}
}

type IncusChildProvisioner struct {
	client                  childIncusClient
	options                 ChildProvisionOptions
	afterStep               func(string) error
	operationWasSubmitted   func(error) bool
	awaitSubmittedOperation func(context.Context, error) error
}

func NewIncusChildProvisioner(client childIncusClient, options ChildProvisionOptions) (*IncusChildProvisioner, error) {
	if client == nil {
		return nil, fmt.Errorf("child client is required for Incus")
	}
	if strings.TrimSpace(options.WorkspacePool) == "" {
		return nil, fmt.Errorf("workspace pool is required")
	}
	if options.CheckProxy == nil {
		return nil, fmt.Errorf("proxy preflight is required")
	}
	if options.WaitRPC == nil {
		return nil, fmt.Errorf("RPC readiness check is required")
	}
	return &IncusChildProvisioner{
		client:                  client,
		options:                 options,
		operationWasSubmitted:   incusclient.OperationWasSubmitted,
		awaitSubmittedOperation: incusclient.AwaitSubmittedOperation,
	}, nil
}

func (p *IncusChildProvisioner) ProvisionChild(ctx context.Context, request ChildRequest) (resources *Resources, resultErr error) {
	if err := request.Workspace.Validate(); err != nil {
		return nil, contract.NewError(contract.ErrorInvalidRequest, err.Error())
	}
	instanceName := "session-" + request.SessionID
	volumeName := "workspace-" + request.SessionID
	if err := validateChildRequestNames(request, instanceName, volumeName); err != nil {
		return nil, err
	}

	var ownership Ownership
	var cleanupResources *Resources
	var submittedErrors []error
	defer func() {
		if resultErr == nil {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), childCleanupTimeout)
		defer cancel()
		for _, submittedErr := range submittedErrors {
			if awaitErr := p.awaitSubmittedOperation(cleanupCtx, submittedErr); awaitErr != nil {
				resultErr = errors.Join(resultErr, awaitErr)
			}
		}
		if cleanupErr := p.cleanupOwned(cleanupCtx, cleanupResources, request.SessionID, ownership.Snapshot()); cleanupErr != nil {
			resultErr = errors.Join(resultErr, cleanupErr)
		}
	}()

	if err := p.options.CheckProxy(ctx); err != nil {
		return nil, contract.NewError(contract.ErrorProxyUnavailable, fmt.Sprintf("configured proxy listener is unavailable: %v", err))
	}
	if err := p.step("check configured proxy listener"); err != nil {
		return nil, err
	}

	poolName, err := p.client.ResolvePool(ctx, p.options.WorkspacePool)
	if err != nil {
		return nil, fmt.Errorf("resolve child workspace pool: %w", err)
	}
	resources = &Resources{SessionID: request.SessionID, Pool: poolName, Instance: instanceName, Volume: volumeName}
	cleanupResources = resources
	if err := p.step("resolve workspace pool"); err != nil {
		return nil, err
	}

	parent, _, err := p.client.GetInstance(ctx, request.SourceInstance)
	if err != nil {
		return nil, fmt.Errorf("get parent Incus instance %q: %w", request.SourceInstance, err)
	}
	rootPool, err := incusclient.EffectiveRootPool(parent)
	if err != nil {
		return nil, err
	}
	if err := p.step("resolve parent effective root pool"); err != nil {
		return nil, err
	}

	if rootPool != poolName {
		return nil, fmt.Errorf("parent root pool %q differs from workspace pool %q; cross-pool child clones are not attested", rootPool, poolName)
	}
	pool, err := p.client.GetStoragePool(ctx, poolName)
	if err != nil {
		return nil, fmt.Errorf("get child clone storage pool %q: %w", poolName, err)
	}
	if err := incusclient.ValidateCOWPool(pool); err != nil {
		return nil, err
	}
	if pool.Name != poolName {
		return nil, fmt.Errorf("storage pool lookup for %q returned pool %q", poolName, pool.Name)
	}
	if err := p.step("require the same created Btrfs pool for root and volume"); err != nil {
		return nil, err
	}

	if parent.Name != request.SourceInstance {
		return nil, fmt.Errorf("parent Incus instance lookup returned %q, want %q", parent.Name, request.SourceInstance)
	}
	parentVolume, _, err := p.client.GetStorageVolumeWithETag(ctx, poolName, request.SourceVolume)
	if err != nil {
		return nil, fmt.Errorf("get parent workspace volume %q: %w", request.SourceVolume, err)
	}
	if parentVolume == nil {
		return nil, fmt.Errorf("get parent workspace volume %q: returned no volume", request.SourceVolume)
	}
	if err := p.step("verify parent instance and volume"); err != nil {
		return nil, err
	}

	if err := p.client.CopyStorageVolume(ctx, poolName, request.SourceVolume, volumeName); err != nil {
		if p.operationWasSubmitted(err) {
			ownership.RecordVolumeSubmitted(volumeName)
			submittedErrors = append(submittedErrors, err)
		}
		return nil, fmt.Errorf("copy child workspace volume: %w", err)
	}
	ownership.RecordVolumeSubmitted(volumeName)
	ownership.RecordVolumeConfirmed()
	if err := p.step("copy child workspace volume"); err != nil {
		return nil, err
	}

	if err := p.client.CopyInstance(ctx, request.SourceInstance, instanceName); err != nil {
		if p.operationWasSubmitted(err) {
			ownership.RecordInstanceSubmitted(instanceName)
			submittedErrors = append(submittedErrors, err)
		}
		return nil, fmt.Errorf("copy stopped child instance: %w", err)
	}
	ownership.RecordInstanceSubmitted(instanceName)
	ownership.RecordInstanceConfirmed()
	if err := p.step("copy stopped child instance"); err != nil {
		return nil, err
	}

	child, childETag, err := p.client.GetInstance(ctx, instanceName)
	if err != nil {
		return nil, fmt.Errorf("get copied child instance %q: %w", instanceName, err)
	}
	if child == nil {
		return nil, fmt.Errorf("get copied child instance %q: returned no instance", instanceName)
	}
	if child.IsActive() {
		return nil, fmt.Errorf("copied child instance %q is active before device replacement", instanceName)
	}
	put := copyWritableInstance(child.Writable())
	put.Devices["workspace"] = map[string]string{
		"type": "disk", "pool": poolName, "source": volumeName, "path": "/workspace",
	}
	if err := p.step("replace workspace device"); err != nil {
		return nil, err
	}
	put.Devices["supervisor"] = map[string]string{
		"type": "proxy", "bind": "instance", "listen": "unix:" + guestSupervisorSocket,
		"connect": "unix:" + request.HostSocketPath, "uid": "1000", "gid": "1000", "mode": "0600",
	}
	if err := p.step("replace supervisor proxy device"); err != nil {
		return nil, err
	}
	applyChildConfig(put.Config, request, volumeName)
	if err := p.client.UpdateInstance(ctx, instanceName, put, childETag); err != nil {
		if p.operationWasSubmitted(err) {
			submittedErrors = append(submittedErrors, err)
		}
		return nil, fmt.Errorf("write child instance metadata and devices: %w", err)
	}
	if err := p.step("write child instance metadata"); err != nil {
		return nil, err
	}

	volume, volumeETag, err := p.client.GetStorageVolumeWithETag(ctx, poolName, volumeName)
	if err != nil {
		return nil, fmt.Errorf("get copied child workspace volume %q: %w", volumeName, err)
	}
	if volume == nil {
		return nil, fmt.Errorf("get copied child workspace volume %q: returned no volume", volumeName)
	}
	volumePut := volume.Writable()
	volumePut.Config = copyConfig(volumePut.Config)
	applyChildMetadata(volumePut.Config, request, volumeName)
	if err := p.client.UpdateStorageVolume(ctx, poolName, volumeName, volumePut, volumeETag); err != nil {
		return nil, fmt.Errorf("write child volume metadata: %w", err)
	}
	if err := p.step("write child volume metadata"); err != nil {
		return nil, err
	}

	verified, _, err := p.client.GetInstance(ctx, instanceName)
	if err != nil {
		return nil, fmt.Errorf("verify child local devices: %w", err)
	}
	if verified == nil {
		return nil, fmt.Errorf("verify child local devices: returned no instance")
	}
	if err := verifyChildDevices(verified.Devices, poolName, volumeName, request.HostSocketPath); err != nil {
		return nil, err
	}
	if err := p.step("verify local devices"); err != nil {
		return nil, err
	}

	startErr := p.client.StartInstance(ctx, instanceName)
	if isDuplicateNICMACError(startErr) {
		startErr = p.regenerateNICMACAndRetryStart(ctx, instanceName, startErr)
	}
	if startErr != nil {
		if p.operationWasSubmitted(startErr) {
			submittedErrors = append(submittedErrors, startErr)
		}
		return nil, fmt.Errorf("start child instance %q: %w", instanceName, startErr)
	}
	if err := p.step("start child instance"); err != nil {
		return nil, err
	}
	if err := prepareSessionWorkspace(ctx, p.client, instanceName, request.Workspace); err != nil {
		return nil, err
	}
	resources.RPCAddr, err = p.options.WaitRPC(ctx, instanceName)
	if err != nil {
		return nil, fmt.Errorf("wait for child RPC address: %w", err)
	}
	if err := p.step("wait for RPC address"); err != nil {
		return nil, err
	}
	return resources, nil
}

func isDuplicateNICMACError(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "Failed start validation for device") &&
		strings.Contains(message, "MAC address") &&
		strings.Contains(message, "already defined on another NIC")
}

func (p *IncusChildProvisioner) regenerateNICMACAndRetryStart(ctx context.Context, instanceName string, collisionErr error) error {
	instance, etag, err := p.client.GetInstance(ctx, instanceName)
	if err != nil {
		return errors.Join(collisionErr, fmt.Errorf("get child instance for NIC MAC regeneration: %w", err))
	}
	if instance == nil {
		return errors.Join(collisionErr, fmt.Errorf("get child instance for NIC MAC regeneration: returned no instance"))
	}
	put := copyWritableInstance(instance.Writable())
	regenerated := false
	for key := range put.Config {
		if strings.HasPrefix(key, "volatile.") && strings.HasSuffix(key, ".hwaddr") {
			delete(put.Config, key)
			regenerated = true
		}
	}
	if !regenerated {
		return collisionErr
	}
	if err := p.client.UpdateInstance(ctx, instanceName, put, etag); err != nil {
		return errors.Join(collisionErr, fmt.Errorf("clear colliding child NIC MAC: %w", err))
	}
	if err := p.client.StartInstance(ctx, instanceName); err != nil {
		return errors.Join(collisionErr, fmt.Errorf("retry child start with regenerated NIC MAC: %w", err))
	}
	return nil
}

func (p *IncusChildProvisioner) Destroy(ctx context.Context, resources *Resources) error {
	if resources == nil {
		return nil
	}
	var errs []error
	if resources.Instance != "" {
		if err := p.deleteOwnedInstance(ctx, resources.Instance); err != nil {
			errs = append(errs, err)
		}
	}
	if resources.Volume != "" {
		if err := p.client.DeleteStorageVolume(ctx, resources.Pool, resources.Volume); err != nil && !incusclient.IsNotFound(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (p *IncusChildProvisioner) cleanupOwned(ctx context.Context, resources *Resources, sessionID string, snapshot OwnershipSnapshot) error {
	if resources == nil {
		return nil
	}
	var errs []error
	if snapshot.Instance.Submitted {
		if err := p.deleteOwnedInstance(ctx, snapshot.Instance.Name); err != nil {
			errs = append(errs, err)
		}
	}
	if snapshot.Volume.Submitted {
		shouldDelete := snapshot.Volume.Confirmed
		metadataSessionID := ""
		if !shouldDelete {
			volume, _, err := p.client.GetStorageVolumeWithETag(ctx, resources.Pool, snapshot.Volume.Name)
			switch {
			case err == nil && volume != nil:
				metadataSessionID = volume.Config[metaSessionID]
				shouldDelete = true
			case err == nil:
				errs = append(errs, fmt.Errorf("probe possibly owned child volume %q: returned no volume", snapshot.Volume.Name))
			case incusclient.IsNotFound(err):
			default:
				errs = append(errs, fmt.Errorf("probe possibly owned child volume %q: %w", snapshot.Volume.Name, err))
			}
		}
		if shouldDelete {
			if err := p.client.DeleteStorageVolume(ctx, resources.Pool, snapshot.Volume.Name); err != nil && !incusclient.IsNotFound(err) {
				errs = append(errs, fmt.Errorf("delete owned child volume %q (metadata session %q, expected %q): %w", snapshot.Volume.Name, metadataSessionID, sessionID, err))
			}
		}
	}
	return errors.Join(errs...)
}

func (p *IncusChildProvisioner) deleteOwnedInstance(ctx context.Context, name string) error {
	instance, _, err := p.client.GetInstance(ctx, name)
	if incusclient.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("probe owned child instance %q: %w", name, err)
	}
	if instance == nil {
		return fmt.Errorf("probe owned child instance %q: returned no instance", name)
	}
	// The read probes both the deterministic name and recovery metadata. An
	// accepted non-refresh copy owns the target even while it still carries the
	// parent's metadata, so metadata cannot safely be used as a deletion gate.
	metadataSessionID := instance.Config[metaSessionID]
	if instance.IsActive() {
		if err := p.client.StopInstance(ctx, name, true); err != nil && !incusclient.IsNotFound(err) {
			return fmt.Errorf("stop owned child instance %q: %w", name, err)
		}
	}
	if err := p.client.DeleteInstance(ctx, name); err != nil && !incusclient.IsNotFound(err) {
		return fmt.Errorf("delete owned child instance %q (metadata session %q): %w", name, metadataSessionID, err)
	}
	return nil
}

func (p *IncusChildProvisioner) step(name string) error {
	if p.afterStep == nil {
		return nil
	}
	if err := p.afterStep(name); err != nil {
		return fmt.Errorf("after %s: %w", name, err)
	}
	return nil
}

func validateChildRequestNames(request ChildRequest, instanceName, volumeName string) error {
	for label, value := range map[string]string{"child instance": instanceName, "child volume": volumeName} {
		if !incusclient.ValidIncusName(value) {
			return fmt.Errorf("%s name %q is not a valid Incus name", label, value)
		}
	}
	if strings.TrimSpace(request.HostSocketPath) == "" {
		return fmt.Errorf("child host socket path is required")
	}
	return nil
}

func copyWritableInstance(put api.InstancePut) api.InstancePut {
	put.Config = copyConfig(put.Config)
	devices := make(api.DevicesMap, len(put.Devices)+2)
	for name, device := range put.Devices {
		devices[name] = copyConfig(device)
	}
	put.Devices = devices
	return put
}

func copyConfig(source api.ConfigMap) api.ConfigMap {
	result := make(api.ConfigMap, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func applyChildMetadata(config api.ConfigMap, request ChildRequest, volumeName string) {
	config[metaSessionID] = request.SessionID
	config[metaParentID] = request.ParentID
	config[metaRootID] = request.RootID
	config[metaKind] = string(request.Contract.Kind)
	config[metaContext] = string(request.Contract.Context)
	config[metaWorker] = request.Contract.WorkerType
	config[metaVolume] = volumeName
	if request.RunAttribution != "" {
		config[metaRun] = request.RunAttribution
	}
}

func applyChildConfig(config api.ConfigMap, request ChildRequest, volumeName string) {
	for key := range config {
		if strings.HasPrefix(key, "environment.KANEDIAS_") {
			delete(config, key)
		}
	}
	applyChildMetadata(config, request, volumeName)
	config["environment.KANEDIAS_SESSION_ID"] = request.SessionID
	config["environment.KANEDIAS_SESSION_KIND"] = string(request.Contract.Kind)
	config["environment.KANEDIAS_WORKER_TYPE"] = request.Contract.WorkerType
	config["environment.KANEDIAS_PI_PROVIDER"] = request.Worker.Provider
	config["environment.KANEDIAS_PI_MODEL"] = request.Worker.Model
	config["environment.KANEDIAS_PI_THINKING"] = request.Worker.ThinkingLevel
	config["environment.KANEDIAS_SUPERVISOR_SOCKET"] = guestSupervisorSocket
	config["environment.KANEDIAS_PI_SESSION_FILE"] = ""
	config["environment.KANEDIAS_PI_WORKDIR"] = request.Workspace.Directory()
	if request.RunAttribution != "" {
		config["environment.KANEDIAS_E2E_RUN_ID"] = request.RunAttribution
	}
	if request.Contract.Context == contract.ContextFork && request.Contract.Fork != nil {
		config["environment.KANEDIAS_PI_SESSION_FILE"] = request.Contract.Fork.SessionFile
	}
}

func verifyChildDevices(devices api.DevicesMap, poolName, volumeName, hostSocket string) error {
	wantWorkspace := map[string]string{"type": "disk", "pool": poolName, "source": volumeName, "path": "/workspace"}
	wantSupervisor := map[string]string{
		"type": "proxy", "bind": "instance", "listen": "unix:" + guestSupervisorSocket,
		"connect": "unix:" + hostSocket, "uid": "1000", "gid": "1000", "mode": "0600",
	}
	if !equalDevice(devices["workspace"], wantWorkspace) {
		return fmt.Errorf("child workspace device was not replaced safely")
	}
	if !equalDevice(devices["supervisor"], wantSupervisor) {
		return fmt.Errorf("child supervisor proxy device was not replaced safely")
	}
	return nil
}

func equalDevice(got map[string]string, want map[string]string) bool {
	if len(got) != len(want) {
		return false
	}
	for key, value := range want {
		if got[key] != value {
			return false
		}
	}
	return true
}

var _ ChildProvisioner = (*IncusChildProvisioner)(nil)
