//go:build incus

package provision

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sklarsa/kanedias/internal/config"
	"github.com/sklarsa/kanedias/internal/incusclient"
	"github.com/sklarsa/kanedias/internal/supervisor/contract"
)

// TestLiveBtrfsChildClone is intentionally opt-in because it clones and starts
// real Incus resources. The named parent must be a running Kanedias image with
// its named custom volume mounted at /workspace.
func TestLiveBtrfsChildClone(t *testing.T) {
	if os.Getenv("KANEDIAS_LIVE_SUPERVISOR") != "1" {
		t.Skip("set KANEDIAS_LIVE_SUPERVISOR=1 to run destructive Incus clone validation")
	}
	parentInstance := requireLiveEnv(t, "KANEDIAS_LIVE_PARENT_INSTANCE")
	parentVolume := requireLiveEnv(t, "KANEDIAS_LIVE_PARENT_VOLUME")
	pool := requireLiveEnv(t, "KANEDIAS_LIVE_BTRFS_POOL")
	proxyAddress := requireLiveEnv(t, "KANEDIAS_LIVE_PROXY_ADDRESS")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	client, err := incusclient.Connect(ctx)
	if err != nil {
		t.Skipf("live Incus environment unavailable: %v", err)
	}
	defer client.Disconnect()

	parent, _, err := client.GetInstance(ctx, parentInstance)
	if err != nil {
		t.Skipf("live parent instance unavailable: %v", err)
	}
	if !parent.IsActive() {
		t.Skipf("live parent instance %q must be running", parentInstance)
	}
	if _, _, err := client.GetStorageVolumeWithETag(ctx, pool, parentVolume); err != nil {
		t.Skipf("live parent volume unavailable: %v", err)
	}

	sessionID := fmt.Sprintf("live-%d-%d", os.Getpid(), time.Now().UnixNano())
	hostSocket := filepath.Join(os.TempDir(), sessionID+".sock")
	listener, err := net.Listen("unix", hostSocket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close(); _ = os.Remove(hostSocket) }()

	request := ChildRequest{
		SessionID: sessionID, ParentID: parent.Config[metaSessionID], RootID: parent.Config[metaRootID],
		SourceInstance: parentInstance, SourceVolume: parentVolume, HostSocketPath: hostSocket,
		Worker:   config.WorkerProfile{Provider: "live", Model: "live", ThinkingLevel: "off"},
		Contract: contract.CreateChildRequest{WorkerType: "live", Kind: contract.ChildKindRead, Context: contract.ContextFresh, Task: "live clone validation"},
	}
	options := ChildProvisionOptions{
		WorkspacePool: pool,
		CheckProxy: func(ctx context.Context) error {
			connection, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "tcp", proxyAddress)
			if err != nil {
				return err
			}
			return connection.Close()
		},
		WaitRPC: func(ctx context.Context, instance string) (string, error) {
			state, err := client.GetInstanceState(ctx, instance)
			if err != nil {
				return "", err
			}
			if state.Status != "Running" {
				return "", fmt.Errorf("instance status is %q", state.Status)
			}
			return "live-ready", nil
		},
	}
	provisioner, err := NewIncusChildProvisioner(client, options)
	if err != nil {
		t.Fatal(err)
	}
	provisioner.afterStep = func(step string) error {
		if step != "copy stopped child instance" {
			return nil
		}
		child, _, err := client.GetInstance(ctx, "session-"+sessionID)
		if err != nil {
			return err
		}
		if child.IsActive() {
			return errors.New("copied child was active before device replacement")
		}
		return nil
	}

	resources, err := provisioner.ProvisionChild(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		if err := provisioner.Destroy(cleanupCtx, resources); err != nil {
			t.Errorf("cleanup live child: %v", err)
		}
	}()

	marker := ".kanedias-live-divergence-" + sessionID
	if _, stderr, err := client.Exec(ctx, resources.Instance, incusclient.ExecRequest{Command: []string{"sh", "-c", "printf child > /workspace/" + marker}}); err != nil {
		t.Fatalf("write child workspace marker: %v (%s)", err, stderr)
	}
	if _, _, err := client.Exec(ctx, parentInstance, incusclient.ExecRequest{Command: []string{"sh", "-c", "test ! -e /workspace/" + marker}}); err != nil {
		t.Fatal("child workspace write appeared in parent volume; clone did not diverge")
	}

	child, _, err := client.GetInstance(ctx, resources.Instance)
	if err != nil {
		t.Fatal(err)
	}
	childConnect := child.Devices["supervisor"]["connect"]
	parentConnect := parent.ExpandedDevices["supervisor"]["connect"]
	if childConnect != "unix:"+hostSocket || childConnect == parentConnect {
		t.Fatalf("child supervisor route = %q, parent = %q", childConnect, parentConnect)
	}
	accepted := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if err == nil {
			_ = connection.Close()
		}
		accepted <- err
	}()
	_, stderr, err := client.Exec(ctx, resources.Instance, incusclient.ExecRequest{Command: []string{
		"node", "-e", `const n=require("net");const s=n.connect("/run/kanedias/supervisor.sock",()=>s.end("ping"));s.on("error",e=>{console.error(e.message);process.exit(1)})`,
	}})
	if err != nil {
		t.Fatalf("connect through replaced child supervisor device: %v (%s)", err, stderr)
	}
	select {
	case err := <-accepted:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("child did not reach its replacement supervisor socket")
	}

	// Cancellation immediately after each completed remote clone exercises the
	// partial-ownership cleanup paths against real named resources.
	for _, cancelAfter := range []string{"copy child workspace volume", "copy stopped child instance"} {
		cancelID := fmt.Sprintf("%s-c%d", sessionID, len(cancelAfter))
		cancelRequest := request
		cancelRequest.SessionID = cancelID
		cancelProvisioner, err := NewIncusChildProvisioner(client, options)
		if err != nil {
			t.Fatal(err)
		}
		cancelProvisioner.afterStep = func(step string) error {
			if step == cancelAfter {
				return context.Canceled
			}
			return nil
		}
		if _, err := cancelProvisioner.ProvisionChild(ctx, cancelRequest); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel after %q error = %v", cancelAfter, err)
		}
		assertLiveResourceAbsent(t, ctx, client, pool, "session-"+cancelID, "workspace-"+cancelID)
	}

	// A closed configured listener must fail before either clone is submitted.
	closedListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedAddress := closedListener.Addr().String()
	_ = closedListener.Close()
	proxyID := sessionID + "-proxy"
	proxyRequest := request
	proxyRequest.SessionID = proxyID
	closedOptions := options
	closedOptions.CheckProxy = func(ctx context.Context) error {
		connection, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp", closedAddress)
		if err == nil {
			_ = connection.Close()
		}
		return err
	}
	closedProvisioner, err := NewIncusChildProvisioner(client, closedOptions)
	if err != nil {
		t.Fatal(err)
	}
	_, err = closedProvisioner.ProvisionChild(ctx, proxyRequest)
	var contractErr *contract.Error
	if !errors.As(err, &contractErr) || contractErr.Code != contract.ErrorProxyUnavailable {
		t.Fatalf("closed proxy error = %v, want proxy_unavailable", err)
	}
	assertLiveResourceAbsent(t, ctx, client, pool, "session-"+proxyID, "workspace-"+proxyID)

	if err := provisioner.Destroy(ctx, resources); err != nil {
		t.Fatal(err)
	}
	resources = nil
	if _, _, err := client.GetInstance(ctx, parentInstance); err != nil {
		t.Fatalf("parent instance missing after child deletion: %v", err)
	}
	if _, _, err := client.GetStorageVolumeWithETag(ctx, pool, parentVolume); err != nil {
		t.Fatalf("parent volume missing after child deletion: %v", err)
	}
}

func requireLiveEnv(t *testing.T, name string) string {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		t.Skipf("%s is required for live supervisor validation", name)
	}
	return value
}

func assertLiveResourceAbsent(t *testing.T, ctx context.Context, client *incusclient.Client, pool, instance, volume string) {
	t.Helper()
	if _, _, err := client.GetInstance(ctx, instance); err == nil || !incusclient.IsNotFound(err) {
		t.Fatalf("instance %q leaked: %v", instance, err)
	}
	if _, _, err := client.GetStorageVolumeWithETag(ctx, pool, volume); err == nil || !incusclient.IsNotFound(err) {
		t.Fatalf("volume %q leaked: %v", volume, err)
	}
}
