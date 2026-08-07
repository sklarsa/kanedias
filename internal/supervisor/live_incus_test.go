//go:build incus

package supervisor_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sklarsa/kanedias/internal/config"
	"github.com/sklarsa/kanedias/internal/incusclient"
	"github.com/sklarsa/kanedias/internal/supervisor"
)

func TestLiveFreshReadDelegation(t *testing.T) {
	if os.Getenv("KANEDIAS_LIVE_SUPERVISOR") != "1" {
		t.Skip("missing prerequisite: KANEDIAS_LIVE_SUPERVISOR=1")
	}
	configPath := os.Getenv("KANEDIAS_CONFIG")
	if configPath == "" {
		configPath = "./config.toml"
	}
	absoluteConfig, err := filepath.Abs(configPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(absoluteConfig)
	if err != nil {
		t.Fatalf("missing prerequisite: load KANEDIAS_CONFIG: %v", err)
	}
	if err := cfg.ValidateSupervisor(); err != nil {
		t.Fatalf("missing prerequisite: valid supervisor config: %v", err)
	}
	worker, ok := cfg.Workers["reviewer"]
	if !ok {
		t.Fatal("missing prerequisite: configured reviewer worker")
	}
	incus, err := incusclient.Connect(context.Background())
	if err != nil {
		t.Fatalf("missing prerequisite: Incus project: %v", err)
	}
	defer incus.Disconnect()
	pool, err := incus.ResolvePool(context.Background(), cfg.Workspace.Pool)
	if err != nil {
		t.Fatalf("missing prerequisite: workspace pool: %v", err)
	}
	storagePool, err := incus.GetStoragePool(context.Background(), pool)
	if err != nil {
		t.Fatalf("missing prerequisite: storage pool: %v", err)
	}
	if err := incusclient.ValidateCOWPool(storagePool); err != nil {
		t.Fatalf("missing prerequisite: attested Btrfs pool: %v", err)
	}
	if _, err := incus.GetImageAlias(context.Background(), cfg.BaseImage.Name); err != nil {
		t.Fatalf("missing prerequisite: base image %q: %v", cfg.BaseImage.Name, err)
	}
	seed := cfg.Workspace.Volume
	if seed == "" {
		seed = config.DefaultWorkspaceVolume
	}
	if _, _, err := incus.GetStorageVolumeWithETag(context.Background(), pool, seed); err != nil {
		t.Fatalf("missing prerequisite: workspace seed %q: %v", seed, err)
	}

	dir := t.TempDir()
	binary := filepath.Join(dir, "kanedias-under-test")
	build := exec.Command("go", "build", "-o", binary, ".")
	build.Dir = filepath.Join("..", "..")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build current checkout: %v: %s", err, output)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	proxy := exec.CommandContext(ctx, binary, "--config", absoluteConfig, "proxy", "run")
	var proxyLog bytes.Buffer
	proxy.Stdout, proxy.Stderr = &proxyLog, &proxyLog
	if err := proxy.Start(); err != nil {
		t.Fatal(err)
	}
	defer stopOwnedProcess(t, proxy, &proxyLog)
	prefix, _ := cfg.Network.IPv4Prefix()
	gateway := net.JoinHostPort(prefix.Addr().String(), "3128")
	poll(t, 30*time.Second, func() bool {
		conn, err := net.DialTimeout("tcp", gateway, 250*time.Millisecond)
		if err != nil {
			return false
		}
		conn.Close()
		return true
	}, "owned proxy listener at "+gateway)

	socket := filepath.Join(dir, "root.sock")
	root := exec.CommandContext(ctx, binary, "--config", absoluteConfig, "session", "--socket", socket)
	var rootLog bytes.Buffer
	root.Stdout, root.Stderr = &rootLog, &rootLog
	if err := root.Start(); err != nil {
		t.Fatal(err)
	}
	defer stopOwnedProcess(t, root, &rootLog)
	client := unixHTTPClient(socket)
	var tree supervisor.NodeSnapshot
	poll(t, 2*time.Minute, func() bool {
		return unixJSON(client, http.MethodGet, "/v1/tree", nil, &tree) == nil && tree.Lifecycle == "ready"
	}, "root supervisor readiness")
	rootID := tree.SessionID
	if rootID == "" || tree.PiSessionID == "" {
		t.Fatalf("root identity not bound: %#v", tree)
	}

	task := fmt.Sprintf("Use delegate_session exactly once with workerType reviewer, kind read, context fresh, and task %q. Return its output.", "Reply with exactly LIVE_READ_OK.")
	var accepted map[string]any
	if err := unixJSON(client, http.MethodPost, "/v1/sessions/"+rootID+"/rpc", map[string]any{"type": "prompt", "message": task}, &accepted); err != nil {
		t.Fatal(err)
	}
	if accepted["success"] != true {
		t.Fatalf("root prompt rejected: %#v", accepted)
	}

	var child supervisor.NodeSnapshot
	poll(t, 4*time.Minute, func() bool {
		var current supervisor.NodeSnapshot
		if unixJSON(client, http.MethodGet, "/v1/tree", nil, &current) != nil {
			return false
		}
		if len(current.Children) == 0 {
			return false
		}
		child = current.Children[0]
		return true
	}, "visible fresh read child")
	if child.SessionID == rootID || child.PiSessionID == tree.PiSessionID {
		t.Fatalf("root and child identities are not distinct: root=%#v child=%#v", tree, child)
	}
	if child.WorkerType != "reviewer" || child.Model.Provider != worker.Provider || child.Model.Model != worker.Model || child.Model.ThinkingLevel != worker.ThinkingLevel {
		t.Fatalf("selected reviewer model mismatch: %#v want %#v", child.Model, worker)
	}
	childInstance := "session-" + child.SessionID
	childVolume := "workspace-" + child.SessionID
	if instance, _, err := incus.GetInstance(context.Background(), childInstance); err != nil || instance.Config["user.kanedias.session_id"] != child.SessionID {
		t.Fatalf("child instance evidence missing: %v %#v", err, instance)
	}
	if volume, _, err := incus.GetStorageVolumeWithETag(context.Background(), pool, childVolume); err != nil || volume.Config["user.kanedias.session_id"] != child.SessionID {
		t.Fatalf("child volume evidence missing: %v %#v", err, volume)
	}

	poll(t, 4*time.Minute, func() bool {
		var current supervisor.NodeSnapshot
		return unixJSON(client, http.MethodGet, "/v1/tree", nil, &current) == nil && current.Lifecycle == "ready" && len(current.Children) == 0
	}, "root settlement and child disappearance")
	var final struct {
		Data struct {
			Text *string `json:"text"`
		} `json:"data"`
	}
	if err := unixJSON(client, http.MethodPost, "/v1/sessions/"+rootID+"/rpc", map[string]any{"type": "get_last_assistant_text"}, &final); err != nil {
		t.Fatal(err)
	}
	if final.Data.Text == nil || !strings.Contains(*final.Data.Text, "LIVE_READ_OK") {
		t.Fatalf("unexpected live answer: %#v", final.Data.Text)
	}
	if !eventReplayContainsChild(t, client, child.SessionID) {
		t.Fatalf("root event replay contains no child events for %s", child.SessionID)
	}
	poll(t, time.Minute, func() bool {
		_, _, instanceErr := incus.GetInstance(context.Background(), childInstance)
		_, _, volumeErr := incus.GetStorageVolumeWithETag(context.Background(), pool, childVolume)
		return incusclient.IsNotFound(instanceErr) && incusclient.IsNotFound(volumeErr)
	}, "child Incus resource cleanup")
}

func unixHTTPClient(socket string) *http.Client {
	return &http.Client{Transport: &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}}
}
func unixJSON(client *http.Client, method, path string, input, output any) error {
	var body io.Reader
	if input != nil {
		encoded, _ := json.Marshal(input)
		body = bytes.NewReader(encoded)
	}
	request, _ := http.NewRequest(method, "http://unix"+path, body)
	request.Header.Set("accept", "application/json")
	if input != nil {
		request.Header.Set("content-type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return err
	}
	if response.StatusCode/100 != 2 {
		return fmt.Errorf("%s: %s", response.Status, data)
	}
	if output != nil {
		return json.Unmarshal(data, output)
	}
	return nil
}
func eventReplayContainsChild(t *testing.T, client *http.Client, childID string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://unix/v1/events", nil)
	response, err := client.Do(request)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	scanner := bufio.NewScanner(response.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var event supervisor.EventEnvelope
		if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &event) == nil && event.SessionID == childID {
			return true
		}
	}
	return false
}
func poll(t *testing.T, timeout time.Duration, predicate func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}
func stopOwnedProcess(t *testing.T, command *exec.Cmd, log *bytes.Buffer) {
	t.Helper()
	if command.Process == nil {
		return
	}
	_ = command.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != -1 {
				t.Logf("owned process exit: %v\n%s", err, log.String())
			}
		}
	case <-time.After(30 * time.Second):
		_ = command.Process.Kill()
		<-done
		t.Logf("killed owned process after timeout\n%s", log.String())
	}
}
