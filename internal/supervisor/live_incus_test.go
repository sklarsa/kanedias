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
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	incus "github.com/lxc/incus/v7/client"
	"github.com/lxc/incus/v7/shared/api"
	"github.com/sklarsa/kanedias/internal/config"
	"github.com/sklarsa/kanedias/internal/incusclient"
	"github.com/sklarsa/kanedias/internal/supervisor"
	"github.com/sklarsa/kanedias/internal/supervisor/contract"
)

const (
	liveTimeout     = 30 * time.Minute
	operationPoll   = 100 * time.Millisecond
	metadataSession = "user.kanedias.session_id"
)

// TestLiveRecursiveSupervisorAcceptance is destructive by design, so every
// capability must be explicitly authorized. In particular, enabling the live
// Incus gate alone never authorizes writes to GitHub.
func TestLiveRecursiveSupervisorAcceptance(t *testing.T) {
	prerequisites := []string{
		"KANEDIAS_LIVE_SUPERVISOR=1",
		"KANEDIAS_CONFIG=<absolute-or-relative-config>",
		"KANEDIAS_E2E_PROVIDER_READY=1",
		"KANEDIAS_E2E_DISPOSABLE_GITHUB=1",
		"KANEDIAS_E2E_GITHUB_REPOSITORY=<owner/repository>",
		"KANEDIAS_E2E_GITHUB_REMOTE=<explicit-git-remote>",
	}
	missing := make([]string, 0)
	if os.Getenv("KANEDIAS_LIVE_SUPERVISOR") != "1" {
		missing = append(missing, prerequisites[0])
	}
	if strings.TrimSpace(os.Getenv("KANEDIAS_CONFIG")) == "" {
		missing = append(missing, prerequisites[1])
	}
	if os.Getenv("KANEDIAS_E2E_PROVIDER_READY") != "1" {
		missing = append(missing, prerequisites[2])
	}
	if os.Getenv("KANEDIAS_E2E_DISPOSABLE_GITHUB") != "1" {
		missing = append(missing, prerequisites[3])
	}
	if strings.TrimSpace(os.Getenv("KANEDIAS_E2E_GITHUB_REPOSITORY")) == "" {
		missing = append(missing, prerequisites[4])
	}
	if strings.TrimSpace(os.Getenv("KANEDIAS_E2E_GITHUB_REMOTE")) == "" {
		missing = append(missing, prerequisites[5])
	}
	if len(missing) != 0 {
		t.Skipf("live supervisor acceptance prerequisites absent (no destructive work performed): %s", strings.Join(missing, ", "))
	}

	harness := newLiveAcceptance(t)
	defer harness.close()
	harness.run()
}

type liveAcceptance struct {
	t          *testing.T
	ctx        context.Context
	cancel     context.CancelFunc
	repoRoot   string
	runDir     string
	binary     string
	configPath string
	cfg        config.Config
	pool       string
	repository string
	remote     string
	prefix     string

	rawIncus incus.InstanceServer
	client   *incusclient.Client
	baseline resourceSnapshot

	proxy   *acceptanceProcess
	roots   []*acceptanceProcess
	streams []*sseCapture

	mu       sync.Mutex
	sessions map[string]struct{}
	async    sync.WaitGroup
	success  bool
}

type acceptanceProcess struct {
	cmd      *exec.Cmd
	log      *os.File
	done     chan struct{}
	waitErr  error
	stopOnce sync.Once
}

type resourceRecord struct {
	Name      string            `json:"name"`
	Type      string            `json:"type"`
	Status    string            `json:"status,omitempty"`
	SessionID string            `json:"sessionId,omitempty"`
	Config    map[string]string `json:"kanediasMetadata,omitempty"`
}

type resourceSnapshot struct {
	Instances map[string]resourceRecord `json:"instances"`
	Volumes   map[string]resourceRecord `json:"customVolumes"`
}

type sseCapture struct {
	response *http.Response
	events   chan supervisor.EventEnvelope
	done     chan error
}

func newLiveAcceptance(t *testing.T) *liveAcceptance {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), liveTimeout)
	repoRoot := commandOutput(t, "git", "rev-parse", "--show-toplevel")
	configPath, err := filepath.Abs(os.Getenv("KANEDIAS_CONFIG"))
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		cancel()
		t.Fatalf("load explicit KANEDIAS_CONFIG: %v", err)
	}
	if err := cfg.ValidateSupervisor(); err != nil {
		cancel()
		t.Fatalf("validate live supervisor configuration: %v", err)
	}
	for _, worker := range []string{"reviewer", "worker"} {
		if _, ok := cfg.Workers[worker]; !ok {
			cancel()
			t.Fatalf("live configuration has no %q worker", worker)
		}
	}
	repository := strings.TrimSpace(os.Getenv("KANEDIAS_E2E_GITHUB_REPOSITORY"))
	configured := false
	for _, candidate := range cfg.Workspace.Repos {
		if candidate == repository {
			configured = true
			break
		}
	}
	if !configured {
		cancel()
		t.Fatalf("disposable repository %q is not present in workspace.repos", repository)
	}

	client, err := incusclient.Connect(ctx)
	if err != nil {
		cancel()
		t.Fatalf("connect to explicit live Incus prerequisite: %v", err)
	}
	pool, err := client.ResolvePool(ctx, cfg.Workspace.Pool)
	if err != nil {
		client.Disconnect()
		cancel()
		t.Fatal(err)
	}
	storagePool, err := client.GetStoragePool(ctx, pool)
	if err != nil {
		client.Disconnect()
		cancel()
		t.Fatal(err)
	}
	if err := incusclient.ValidateCOWPool(storagePool); err != nil {
		client.Disconnect()
		cancel()
		t.Fatalf("live pool is not the approved Btrfs configuration: %v", err)
	}
	if _, err := client.GetImageAlias(ctx, cfg.BaseImage.Name); err != nil {
		client.Disconnect()
		cancel()
		t.Fatalf("live base image prerequisite: %v", err)
	}
	seed := cfg.Workspace.Volume
	if seed == "" {
		seed = config.DefaultWorkspaceVolume
	}
	if _, _, err := client.GetStorageVolumeWithETag(ctx, pool, seed); err != nil {
		client.Disconnect()
		cancel()
		t.Fatalf("live workspace seed prerequisite: %v", err)
	}

	raw, err := incus.ConnectIncusUnixWithContext(ctx, "", nil)
	if err != nil {
		client.Disconnect()
		cancel()
		t.Fatal(err)
	}
	rawIncus := raw.UseProject(incusclient.ProjectName)
	prefix := fmt.Sprintf("e2e-%d-%d", os.Getpid(), time.Now().UnixNano())
	base := strings.TrimSpace(os.Getenv("KANEDIAS_E2E_ARTIFACT_DIR"))
	if base == "" {
		cache, cacheErr := os.UserCacheDir()
		if cacheErr != nil {
			raw.Disconnect()
			client.Disconnect()
			cancel()
			t.Fatal(cacheErr)
		}
		base = filepath.Join(cache, "kanedias", "e2e")
	}
	runDir := filepath.Join(base, prefix)
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		raw.Disconnect()
		client.Disconnect()
		cancel()
		t.Fatal(err)
	}
	if err := os.Chmod(runDir, 0o700); err != nil {
		raw.Disconnect()
		client.Disconnect()
		cancel()
		t.Fatal(err)
	}

	h := &liveAcceptance{
		t: t, ctx: ctx, cancel: cancel, repoRoot: repoRoot, runDir: runDir,
		binary: filepath.Join(runDir, "kanedias-under-test"), configPath: configPath,
		cfg: cfg, pool: pool, repository: repository,
		remote: strings.TrimSpace(os.Getenv("KANEDIAS_E2E_GITHUB_REMOTE")), prefix: prefix,
		rawIncus: rawIncus, client: client, sessions: make(map[string]struct{}),
	}
	h.baseline = h.snapshotResources("baseline")
	h.writeJSON("manifest.json", map[string]any{
		"prefix": prefix, "repository": repository, "remote": h.remote,
		"config": configPath, "pool": pool, "baseline": h.baseline,
	})
	return h
}

func (h *liveAcceptance) run() {
	h.buildReviewedCheckout()
	h.startProxy()

	root, socket, tree, stream, stalled := h.startRoot("main")
	defer stalled.Close()
	h.exerciseRootRPC(tree, socket, stream)
	h.exerciseQuestionFixture()
	h.exerciseFreshRead(tree, socket)
	h.exerciseForkedWrite(tree, socket)
	h.exerciseGracefulCascade(root, tree, socket)
	h.assertBaseline("after-graceful-cascade")

	killedRoot, killedSocket, killedTree, _, killedStalled := h.startRoot("killed")
	defer killedStalled.Close()
	h.exerciseKilledCascade(killedRoot, killedTree, killedSocket)
	h.assertBaseline("after-killed-root-exact-teardown")

	if os.Getenv("KANEDIAS_E2E_EXTERNAL_PROXY") == "1" {
		h.t.Log("external proxy mode: operator-owned listener is preserved; run the default owned-proxy mode for the missing-proxy phase")
	} else {
		h.stopProxy()
		h.exerciseMissingProxy()
	}
	h.assertBaseline("final")
	h.success = true
}

func (h *liveAcceptance) close() {
	for _, root := range h.roots {
		h.stopProcess(root, syscall.SIGTERM, 30*time.Second)
	}
	h.stopProxy()
	for _, stream := range h.streams {
		_ = stream.response.Body.Close()
		select {
		case <-stream.done:
		case <-time.After(time.Second):
		}
	}
	h.async.Wait()
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := h.cleanupExactSessions(cleanupCtx, h.sessionIDs()); err != nil {
		h.t.Errorf("exact metadata teardown: %v", err)
	}
	final := h.snapshotResources("teardown-final")
	if !sameResourceNames(h.baseline, final) {
		h.t.Errorf("Incus baseline mismatch after exact teardown: %s", resourceDiff(h.baseline, final))
	}
	if h.client != nil {
		h.client.Disconnect()
	}
	if h.rawIncus != nil {
		h.rawIncus.Disconnect()
	}
	h.cancel()
	if h.success && !h.t.Failed() {
		if err := os.RemoveAll(h.runDir); err != nil {
			h.t.Errorf("remove successful artifact directory: %v", err)
		}
		return
	}
	h.t.Logf("persistent live acceptance failure artifacts: %s", h.runDir)
}

func (h *liveAcceptance) buildReviewedCheckout() {
	command := exec.CommandContext(h.ctx, "go", "build", "-o", h.binary, ".")
	command.Dir = h.repoRoot
	output, err := command.CombinedOutput()
	_ = os.WriteFile(filepath.Join(h.runDir, "build.log"), output, 0o600)
	if err != nil {
		h.t.Fatalf("build reviewed checkout: %v", err)
	}
	absolute, err := filepath.Abs(h.binary)
	if err != nil || absolute != h.binary {
		h.t.Fatalf("binary under test is not absolute: %q (%v)", h.binary, err)
	}
}

func (h *liveAcceptance) startProxy() {
	prefix, err := h.cfg.Network.IPv4Prefix()
	if err != nil {
		h.t.Fatal(err)
	}
	gateway := net.JoinHostPort(prefix.Addr().String(), "3128")
	if os.Getenv("KANEDIAS_E2E_EXTERNAL_PROXY") == "1" {
		h.poll(30*time.Second, "explicit external proxy at "+gateway, func() bool { return canDialTCP(gateway) })
		return
	}
	h.proxy = h.startProcess("proxy", h.binary, "--config", h.configPath, "proxy", "run")
	h.poll(30*time.Second, "owned proxy at "+gateway, func() bool {
		select {
		case <-h.proxy.done:
			h.t.Fatalf("owned proxy exited before readiness: %v", h.proxy.waitErr)
		default:
		}
		return canDialTCP(gateway)
	})
}

func (h *liveAcceptance) stopProxy() {
	if h.proxy == nil {
		return
	}
	h.stopProcess(h.proxy, syscall.SIGTERM, 30*time.Second)
	h.proxy = nil
}

func (h *liveAcceptance) startRoot(label string) (*acceptanceProcess, string, supervisor.NodeSnapshot, *sseCapture, net.Conn) {
	socket := filepath.Join(h.runDir, label+"-root.sock")
	root := h.startProcess(label+"-root", h.binary, "--config", h.configPath, "session", "--socket", socket)
	h.roots = append(h.roots, root)
	client := unixHTTPClient(socket)
	var tree supervisor.NodeSnapshot
	h.poll(3*time.Minute, label+" root readiness", func() bool {
		return unixJSON(client, http.MethodGet, "/v1/tree", nil, &tree) == nil && tree.Lifecycle == "ready"
	})
	if tree.SessionID == "" || tree.PiSessionID == "" || tree.SessionFile == "" {
		h.t.Fatalf("%s root identity is not fully bound: %#v", label, tree)
	}
	h.trackTree(tree)
	info, err := os.Stat(socket)
	if err != nil {
		h.t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 || info.Mode()&os.ModeSocket == 0 {
		h.t.Fatalf("root socket mode/type = %v, want Unix 0600", info.Mode())
	}
	stream := h.startSSE(label, client)
	stalled, err := net.Dial("unix", socket)
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err := io.WriteString(stalled, "GET /v1/events HTTP/1.1\r\nHost: unix\r\nAccept: text/event-stream\r\n\r\n"); err != nil {
		h.t.Fatal(err)
	}
	h.snapshotTree(label+"-ready", client)
	h.assertSessionMetadata(tree.SessionID)
	return root, socket, tree, stream, stalled
}

func (h *liveAcceptance) exerciseRootRPC(root supervisor.NodeSnapshot, socket string, stream *sseCapture) {
	client := unixHTTPClient(socket)
	marker := "KANEDIAS_E2E_ROOT_OK_" + h.prefix
	h.rpc(client, root.SessionID, map[string]any{"type": "prompt", "message": "Reply with exactly " + marker + "."})
	h.waitPiEvent(stream.events, root.SessionID, "agent_settled", 4*time.Minute)
	// This later RPC proves the consuming and deliberately stalled SSE clients
	// did not apply backpressure to Pi stdout.
	state := h.rpc(client, root.SessionID, map[string]any{"type": "get_state"})
	if state["success"] != true {
		h.t.Fatalf("later get_state failed with stalled SSE client: %#v", state)
	}
	text := h.lastAssistantText(client, root.SessionID)
	if !strings.Contains(text, marker) {
		h.t.Fatalf("root final text %q does not contain %q", text, marker)
	}
}

func (h *liveAcceptance) exerciseQuestionFixture() {
	fixture, err := os.ReadFile(filepath.Join(h.repoRoot, "internal", "supervisor", "testdata", "pi-input.json"))
	if err != nil {
		h.t.Fatal(err)
	}
	sender := &fixtureQuestionSender{}
	store := supervisor.NewQuestionStore(sender)
	retained, err := store.Retain(fixture)
	if err != nil || !retained {
		h.t.Fatalf("retain controlled blocking question: retained=%v err=%v", retained, err)
	}
	broker := supervisor.NewEventBroker()
	for index := 0; index < supervisor.DefaultEventRingCapacity+32; index++ {
		broker.PublishLocal("fixture", "pi", json.RawMessage(`{"type":"noise"}`))
	}
	if len(store.Summaries()) != 1 {
		h.t.Fatal("blocking question did not survive event-ring eviction")
	}
	question := store.Summaries()[0]
	if err := store.Answer(h.ctx, question.ID, json.RawMessage(`{"value":"deterministic-answer"}`)); err != nil {
		h.t.Fatal(err)
	}
	if len(store.Summaries()) != 0 {
		h.t.Fatal("answered question remained pending")
	}
	err = store.Answer(h.ctx, question.ID, json.RawMessage(`{"value":"duplicate"}`))
	var typed *contract.Error
	if !errors.As(err, &typed) || typed.Code != contract.ErrorNotFound {
		h.t.Fatalf("duplicate question response error = %v", err)
	}
	h.writeJSON("question-fixture.json", map[string]any{"question": question, "responses": sender.messages})
}

func (h *liveAcceptance) exerciseFreshRead(root supervisor.NodeSnapshot, socket string) {
	client := unixHTTPClient(socket)
	body, err := os.ReadFile(filepath.Join(h.repoRoot, "internal", "supervisor", "testdata", "read-task.md"))
	if err != nil {
		h.t.Fatal(err)
	}
	prompt := fmt.Sprintf("Use delegate_session exactly once with workerType reviewer, kind read, context fresh, and this exact task: %q. After it returns, reproduce its answer.", strings.TrimSpace(string(body)))
	h.rpc(client, root.SessionID, map[string]any{"type": "prompt", "message": prompt})
	var child supervisor.NodeSnapshot
	h.poll(6*time.Minute, "fresh read child visibility", func() bool {
		var current supervisor.NodeSnapshot
		if unixJSON(client, http.MethodGet, "/v1/tree", nil, &current) != nil || len(current.Children) == 0 {
			return false
		}
		child = current.Children[0]
		return child.Kind == contract.ChildKindRead && child.Context == contract.ContextFresh
	})
	h.trackTree(child)
	h.assertDistinct(root, child)
	h.assertWorker(child, "reviewer")
	h.assertSessionMetadata(child.SessionID)
	for _, command := range []map[string]any{{"type": "get_messages"}, {"type": "get_entries"}, {"type": "steer", "message": "Ensure the exact KANEDIAS_E2E_READ_OK marker is in the final response."}} {
		response := h.rpc(client, child.SessionID, command)
		if response["success"] != true {
			h.t.Fatalf("routed %v failed: %#v", command["type"], response)
		}
	}
	h.poll(8*time.Minute, "fresh child result and cleanup", func() bool {
		var current supervisor.NodeSnapshot
		return unixJSON(client, http.MethodGet, "/v1/tree", nil, &current) == nil && current.Lifecycle == "ready" && len(current.Children) == 0
	})
	if text := h.lastAssistantText(client, root.SessionID); !strings.Contains(text, "KANEDIAS_E2E_READ_OK") {
		h.t.Fatalf("delegate_session read answer missing marker: %q", text)
	}
	h.assertSessionAbsent(child.SessionID)
	if _, err := os.Stat(filepath.Join(h.runDir, child.SessionID+".sock")); !errors.Is(err, os.ErrNotExist) {
		h.t.Fatalf("fresh child socket remains: %v", err)
	}
	h.snapshotTree("fresh-read-complete", client)
}

func (h *liveAcceptance) exerciseForkedWrite(root supervisor.NodeSnapshot, socket string) {
	client := unixHTTPClient(socket)
	rootInstance := h.instanceForSession(root.SessionID)
	checkout := "/workspace/repos/" + h.repository
	beforeSize := h.execInstance(rootInstance, "stat", "-c", "%s", root.SessionFile)
	beforeHash := strings.Fields(h.execInstance(rootInstance, "sha256sum", root.SessionFile))[0]
	base := h.execInstance(rootInstance, "git", "-C", checkout, "rev-parse", "HEAD")
	branch := "kanedias-e2e/" + h.prefix
	marker := "KANEDIAS_E2E_WRITE_" + h.prefix
	body, err := os.ReadFile(filepath.Join(h.repoRoot, "internal", "supervisor", "testdata", "write-task.md"))
	if err != nil {
		h.t.Fatal(err)
	}
	prompt := fmt.Sprintf("Use delegate_session exactly once with workerType worker, kind write, context fork. The task is: %s Repository slug: %s. Checkout: %s. Base commit: %s. Branch: %s. Marker: %s. After handoff, return the full typed result including refs.", strings.TrimSpace(string(body)), h.repository, checkout, base, branch, marker)
	h.rpc(client, root.SessionID, map[string]any{"type": "prompt", "message": prompt})
	var child supervisor.NodeSnapshot
	h.poll(8*time.Minute, "forked writer visibility", func() bool {
		var current supervisor.NodeSnapshot
		if unixJSON(client, http.MethodGet, "/v1/tree", nil, &current) != nil || len(current.Children) == 0 {
			return false
		}
		child = current.Children[0]
		return child.Kind == contract.ChildKindWrite && child.Context == contract.ContextFork
	})
	h.trackTree(child)
	h.assertDistinct(root, child)
	h.assertWorker(child, "worker")
	if child.ParentSessionID != root.SessionID || child.PiSessionID == root.PiSessionID {
		h.t.Fatalf("fork metadata/identity mismatch: root=%#v child=%#v", root, child)
	}
	h.assertSessionMetadata(child.SessionID)
	h.poll(12*time.Minute, "accepted writer handoff and child disappearance", func() bool {
		var current supervisor.NodeSnapshot
		return unixJSON(client, http.MethodGet, "/v1/tree", nil, &current) == nil && current.Lifecycle == "ready" && len(current.Children) == 0
	})
	afterPrefixHash := strings.Fields(h.execInstance(rootInstance, "sh", "-c", fmt.Sprintf("head -c %s -- %q | sha256sum", beforeSize, root.SessionFile)))[0]
	if beforeHash != afterPrefixHash {
		h.t.Fatalf("parent Pi session history was rewritten during fork: before=%q retained-prefix=%q", beforeHash, afterPrefixHash)
	}
	head := strings.Fields(commandOutput(h.t, "git", "ls-remote", "--exit-code", h.remote, "refs/heads/"+branch))
	if len(head) < 2 || head[1] != "refs/heads/"+branch {
		h.t.Fatalf("disposable remote branch did not resolve exactly: %#v", head)
	}
	resultText := h.lastAssistantText(client, root.SessionID)
	if !strings.Contains(resultText, branch) || !strings.Contains(resultText, head[0]) {
		h.t.Fatalf("parent result did not retain verified refs branch=%q head=%q: %s", branch, head[0], resultText)
	}
	h.writeJSON("reported-git-refs.json", map[string]string{"repository": h.repository, "base": base, "branch": branch, "head": head[0]})
	h.assertSessionAbsent(child.SessionID)
	h.snapshotTree("forked-write-complete", client)
}

func (h *liveAcceptance) exerciseGracefulCascade(rootProcess *acceptanceProcess, root supervisor.NodeSnapshot, socket string) {
	client := unixHTTPClient(socket)
	h.startNestedDelegation(client, root.SessionID, "graceful")
	var nested supervisor.NodeSnapshot
	h.poll(8*time.Minute, "child and grandchild before graceful stop", func() bool {
		var current supervisor.NodeSnapshot
		if unixJSON(client, http.MethodGet, "/v1/tree", nil, &current) != nil || len(current.Children) == 0 || len(current.Children[0].Children) == 0 {
			return false
		}
		nested = current
		return true
	})
	h.trackTree(nested)
	pids := descendantPIDs(rootProcess.cmd.Process.Pid)
	status, _, err := unixRequest(client, http.MethodDelete, "/v1/sessions/"+root.SessionID, nil)
	if err != nil || status != http.StatusAccepted {
		h.t.Fatalf("graceful root DELETE = %d, %v", status, err)
	}
	h.waitProcess(rootProcess, 2*time.Minute)
	h.poll(2*time.Minute, "graceful descendant resource cleanup", func() bool { return h.sessionsAbsent(treeSessionIDs(nested)) })
	h.assertPIDsDead(pids)
	if _, err := os.Stat(socket); !errors.Is(err, os.ErrNotExist) {
		h.t.Fatalf("graceful root socket remains: %v", err)
	}
}

func (h *liveAcceptance) exerciseKilledCascade(rootProcess *acceptanceProcess, root supervisor.NodeSnapshot, socket string) {
	client := unixHTTPClient(socket)
	h.startNestedDelegation(client, root.SessionID, "killed")
	var nested supervisor.NodeSnapshot
	h.poll(8*time.Minute, "child and grandchild before SIGKILL", func() bool {
		var current supervisor.NodeSnapshot
		if unixJSON(client, http.MethodGet, "/v1/tree", nil, &current) != nil || len(current.Children) == 0 || len(current.Children[0].Children) == 0 {
			return false
		}
		nested = current
		return true
	})
	h.trackTree(nested)
	pids := descendantPIDs(rootProcess.cmd.Process.Pid)
	if err := rootProcess.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		h.t.Fatal(err)
	}
	h.waitProcess(rootProcess, time.Minute)
	descendants := treeSessionIDs(nested)
	if len(descendants) > 0 {
		descendants = descendants[1:]
	}
	h.poll(2*time.Minute, "liveness EOF descendant cleanup after SIGKILL", func() bool { return h.sessionsAbsent(descendants) })
	h.assertPIDsDead(pids)
	// Approved v1 limitation: a killed root cannot clean its own instance or
	// volume. Teardown targets only resources whose exact metadata matches this
	// root; this is not host-wide crash reconciliation.
	cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := h.cleanupExactSessions(cleanupCtx, []string{root.SessionID}); err != nil {
		h.t.Fatalf("exact killed-root teardown: %v", err)
	}
	_ = os.Remove(socket)
	h.assertSessionAbsent(root.SessionID)
}

func (h *liveAcceptance) exerciseMissingProxy() {
	before := h.snapshotResources("missing-proxy-before")
	socket := filepath.Join(h.runDir, "missing-proxy.sock")
	process := h.startProcess("missing-proxy-root", h.binary, "--config", h.configPath, "session", "--socket", socket)
	h.waitProcess(process, 2*time.Minute)
	logData, _ := os.ReadFile(filepath.Join(h.runDir, "missing-proxy-root.log"))
	if !strings.Contains(string(logData), string(contract.ErrorProxyUnavailable)) && !strings.Contains(strings.ToLower(string(logData)), "proxy") {
		h.t.Fatalf("missing proxy error is not clear: %s", logData)
	}
	after := h.snapshotResources("missing-proxy-after")
	if !sameResourceNames(before, after) {
		h.t.Fatalf("missing proxy created session-owned resources: %s", resourceDiff(before, after))
	}
	if _, err := os.Stat(socket); !errors.Is(err, os.ErrNotExist) {
		h.t.Fatalf("missing-proxy socket remains: %v", err)
	}
}

func (h *liveAcceptance) startNestedDelegation(client *http.Client, rootID, label string) {
	grandchildTask := "Inspect the repository history carefully and return KANEDIAS_E2E_GRANDCHILD_OK only after completing the inspection."
	childTask := fmt.Sprintf("Use delegate_session with workerType reviewer, kind read, context fresh, task %q; wait for it and return its response.", grandchildTask)
	request := map[string]any{"workerType": "reviewer", "kind": "read", "context": "fresh", "task": childTask}
	h.async.Add(1)
	go func() {
		defer h.async.Done()
		status, body, err := unixRequest(client, http.MethodPost, "/v1/sessions/"+rootID+"/children", request)
		h.writeJSON(label+"-nested-delegation-result.json", map[string]any{"status": status, "body": string(body), "error": errorString(err)})
	}()
}

func (h *liveAcceptance) rpc(client *http.Client, sessionID string, command map[string]any) map[string]any {
	var response map[string]any
	if err := unixJSON(client, http.MethodPost, "/v1/sessions/"+sessionID+"/rpc", command, &response); err != nil {
		h.t.Fatalf("RPC %v to %s: %v", command["type"], sessionID, err)
	}
	return response
}

func (h *liveAcceptance) lastAssistantText(client *http.Client, sessionID string) string {
	response := h.rpc(client, sessionID, map[string]any{"type": "get_last_assistant_text"})
	data, _ := response["data"].(map[string]any)
	text, _ := data["text"].(string)
	return text
}

func (h *liveAcceptance) startSSE(label string, client *http.Client) *sseCapture {
	request, _ := http.NewRequestWithContext(h.ctx, http.MethodGet, "http://unix/v1/events", nil)
	request.Header.Set("Accept", "text/event-stream")
	response, err := client.Do(request)
	if err != nil {
		h.t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		h.t.Fatalf("SSE status = %s", response.Status)
	}
	file, err := os.OpenFile(filepath.Join(h.runDir, label+"-events.sse"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		response.Body.Close()
		h.t.Fatal(err)
	}
	capture := &sseCapture{response: response, events: make(chan supervisor.EventEnvelope, 4096), done: make(chan error, 1)}
	h.streams = append(h.streams, capture)
	go func() {
		defer file.Close()
		defer close(capture.events)
		scanner := bufio.NewScanner(io.TeeReader(response.Body, file))
		scanner.Buffer(make([]byte, 64*1024), 1<<20)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			var event supervisor.EventEnvelope
			if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &event) == nil {
				capture.events <- event
			}
		}
		capture.done <- scanner.Err()
	}()
	return capture
}

func (h *liveAcceptance) waitPiEvent(events <-chan supervisor.EventEnvelope, sessionID, eventType string, timeout time.Duration) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case event, ok := <-events:
			if !ok {
				h.t.Fatalf("SSE ended before %s from %s", eventType, sessionID)
			}
			var payload struct {
				Type string `json:"type"`
			}
			if event.SessionID == sessionID && json.Unmarshal(event.Payload, &payload) == nil && payload.Type == eventType {
				return
			}
		case <-timer.C:
			h.t.Fatalf("timed out waiting for %s from %s", eventType, sessionID)
		}
	}
}

func (h *liveAcceptance) snapshotResources(label string) resourceSnapshot {
	server := h.rawIncus
	instances, err := server.GetInstances(api.InstanceTypeAny)
	if err != nil {
		h.t.Fatalf("snapshot Incus instances: %v", err)
	}
	volumes, err := server.GetStoragePoolVolumes(h.pool)
	if err != nil {
		h.t.Fatalf("snapshot Incus volumes: %v", err)
	}
	snapshot := resourceSnapshot{Instances: make(map[string]resourceRecord), Volumes: make(map[string]resourceRecord)}
	for _, instance := range instances {
		snapshot.Instances[instance.Name] = resourceRecord{Name: instance.Name, Type: string(instance.Type), Status: instance.Status, SessionID: instance.Config[metadataSession], Config: kanediasMetadata(instance.Config)}
	}
	for _, volume := range volumes {
		if volume.Type != "custom" {
			continue
		}
		snapshot.Volumes[volume.Name] = resourceRecord{Name: volume.Name, Type: volume.Type, SessionID: volume.Config[metadataSession], Config: kanediasMetadata(volume.Config)}
	}
	h.writeJSON(label+"-incus.json", snapshot)
	return snapshot
}

func (h *liveAcceptance) assertBaseline(label string) {
	current := h.snapshotResources(label)
	if !sameResourceNames(h.baseline, current) {
		h.t.Fatalf("Incus baseline mismatch at %s: %s", label, resourceDiff(h.baseline, current))
	}
}

func (h *liveAcceptance) assertSessionMetadata(sessionID string) {
	h.trackSession(sessionID)
	var instanceFound, volumeFound bool
	snapshot := h.snapshotResources("metadata-" + safeName(sessionID))
	for _, record := range snapshot.Instances {
		if record.SessionID == sessionID {
			instanceFound = true
		}
	}
	for _, record := range snapshot.Volumes {
		if record.SessionID == sessionID {
			volumeFound = true
		}
	}
	if !instanceFound || !volumeFound {
		h.t.Fatalf("session %s metadata incomplete: instance=%v volume=%v", sessionID, instanceFound, volumeFound)
	}
}

func (h *liveAcceptance) assertSessionAbsent(sessionID string) {
	h.poll(time.Minute, "session metadata/resource disappearance for "+sessionID, func() bool { return h.sessionsAbsent([]string{sessionID}) })
}

func (h *liveAcceptance) sessionsAbsent(ids []string) bool {
	wanted := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		wanted[id] = struct{}{}
	}
	server := h.rawIncus
	instances, err := server.GetInstances(api.InstanceTypeAny)
	if err != nil {
		return false
	}
	for _, instance := range instances {
		if _, ok := wanted[instance.Config[metadataSession]]; ok {
			return false
		}
	}
	volumes, err := server.GetStoragePoolVolumes(h.pool)
	if err != nil {
		return false
	}
	for _, volume := range volumes {
		if volume.Type == "custom" {
			if _, ok := wanted[volume.Config[metadataSession]]; ok {
				return false
			}
		}
	}
	return true
}

func (h *liveAcceptance) cleanupExactSessions(ctx context.Context, ids []string) error {
	wanted := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		wanted[id] = struct{}{}
	}
	server := h.rawIncus
	instances, err := server.GetInstances(api.InstanceTypeAny)
	if err != nil {
		return err
	}
	var cleanup []error
	for _, instance := range instances {
		if _, ok := wanted[instance.Config[metadataSession]]; !ok {
			continue
		}
		if instance.IsActive() {
			op, stopErr := server.UpdateInstanceState(instance.Name, api.InstanceStatePut{Action: "stop", Force: true}, "")
			if stopErr == nil {
				stopErr = op.WaitContext(ctx)
			}
			cleanup = append(cleanup, stopErr)
		}
		op, deleteErr := server.DeleteInstance(instance.Name)
		if deleteErr == nil {
			deleteErr = op.WaitContext(ctx)
		}
		cleanup = append(cleanup, deleteErr)
	}
	volumes, err := server.GetStoragePoolVolumes(h.pool)
	if err != nil {
		cleanup = append(cleanup, err)
	} else {
		for _, volume := range volumes {
			if volume.Type != "custom" {
				continue
			}
			if _, ok := wanted[volume.Config[metadataSession]]; !ok {
				continue
			}
			cleanup = append(cleanup, server.DeleteStoragePoolVolume(h.pool, "custom", volume.Name))
		}
	}
	return errors.Join(cleanup...)
}

func (h *liveAcceptance) assertDistinct(root, child supervisor.NodeSnapshot) {
	if root.SessionID == child.SessionID || root.PiSessionID == child.PiSessionID || root.SessionFile == child.SessionFile {
		h.t.Fatalf("root/child session identities are not distinct: root=%#v child=%#v", root, child)
	}
	rootInstance := h.instanceForSession(root.SessionID)
	childInstance := h.instanceForSession(child.SessionID)
	rootVolume := h.volumeForSession(root.SessionID)
	childVolume := h.volumeForSession(child.SessionID)
	if rootInstance == childInstance || rootVolume == childVolume {
		h.t.Fatalf("root/child resources are not distinct: instances=(%s,%s) volumes=(%s,%s)", rootInstance, childInstance, rootVolume, childVolume)
	}
}

func (h *liveAcceptance) assertWorker(child supervisor.NodeSnapshot, workerType string) {
	worker := h.cfg.Workers[workerType]
	if child.WorkerType != workerType || child.Model.Provider != worker.Provider || child.Model.Model != worker.Model || child.Model.ThinkingLevel != worker.ThinkingLevel {
		h.t.Fatalf("worker policy mismatch for %s: child=%#v configured=%#v", workerType, child, worker)
	}
}

func (h *liveAcceptance) instanceForSession(sessionID string) string {
	snapshot := h.snapshotResources("lookup-" + safeName(sessionID))
	for name, record := range snapshot.Instances {
		if record.SessionID == sessionID {
			return name
		}
	}
	h.t.Fatalf("no instance has exact session metadata %q", sessionID)
	return ""
}

func (h *liveAcceptance) volumeForSession(sessionID string) string {
	snapshot := h.snapshotResources("volume-lookup-" + safeName(sessionID))
	for name, record := range snapshot.Volumes {
		if record.SessionID == sessionID {
			return name
		}
	}
	h.t.Fatalf("no custom volume has exact session metadata %q", sessionID)
	return ""
}

func (h *liveAcceptance) execInstance(instance string, command ...string) string {
	stdout, stderr, err := h.client.Exec(h.ctx, instance, incusclient.ExecRequest{Command: command})
	if err != nil {
		h.t.Fatalf("exec %q in %s: %v (%s)", command, instance, err, stderr)
	}
	return strings.TrimSpace(stdout)
}

func (h *liveAcceptance) snapshotTree(label string, client *http.Client) {
	var tree supervisor.NodeSnapshot
	if err := unixJSON(client, http.MethodGet, "/v1/tree", nil, &tree); err == nil {
		h.trackTree(tree)
		h.writeJSON(label+"-tree.json", tree)
	}
}

func (h *liveAcceptance) trackTree(tree supervisor.NodeSnapshot) {
	for _, id := range treeSessionIDs(tree) {
		h.trackSession(id)
	}
}

func (h *liveAcceptance) trackSession(id string) {
	if id == "" {
		return
	}
	h.mu.Lock()
	h.sessions[id] = struct{}{}
	h.mu.Unlock()
}

func (h *liveAcceptance) sessionIDs() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	ids := make([]string, 0, len(h.sessions))
	for id := range h.sessions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (h *liveAcceptance) startProcess(label, executable string, arguments ...string) *acceptanceProcess {
	log, err := os.OpenFile(filepath.Join(h.runDir, label+".log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		h.t.Fatal(err)
	}
	command := exec.Command(executable, arguments...)
	command.Dir = h.repoRoot
	command.Stdout, command.Stderr = log, log
	process := &acceptanceProcess{cmd: command, log: log, done: make(chan struct{})}
	if err := command.Start(); err != nil {
		log.Close()
		h.t.Fatalf("start %s: %v", label, err)
	}
	go func() {
		process.waitErr = command.Wait()
		_ = log.Close()
		close(process.done)
	}()
	return process
}

func (h *liveAcceptance) waitProcess(process *acceptanceProcess, timeout time.Duration) error {
	select {
	case <-process.done:
		return process.waitErr
	case <-time.After(timeout):
		_ = process.cmd.Process.Kill()
		<-process.done
		h.t.Fatalf("process %d did not exit in %s (kill result %v)", process.cmd.Process.Pid, timeout, process.waitErr)
		return process.waitErr
	}
}

func (h *liveAcceptance) stopProcess(process *acceptanceProcess, signal os.Signal, timeout time.Duration) {
	if process == nil || process.cmd.Process == nil {
		return
	}
	process.stopOnce.Do(func() {
		_ = process.cmd.Process.Signal(signal)
		select {
		case <-process.done:
		case <-time.After(timeout):
			_ = process.cmd.Process.Kill()
			<-process.done
		}
	})
}

func (h *liveAcceptance) poll(timeout time.Duration, description string, predicate func() bool) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		select {
		case <-h.ctx.Done():
			h.t.Fatalf("acceptance context ended while waiting for %s: %v", description, h.ctx.Err())
		case <-time.After(operationPoll):
		}
	}
	h.snapshotResources("timeout-" + safeName(description))
	h.t.Fatalf("timed out waiting for %s", description)
}

func (h *liveAcceptance) writeJSON(name string, value any) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		h.t.Errorf("encode artifact %s: %v", name, err)
		return
	}
	if err := os.WriteFile(filepath.Join(h.runDir, name), append(data, '\n'), 0o600); err != nil {
		h.t.Errorf("write artifact %s: %v", name, err)
	}
}

func (h *liveAcceptance) assertPIDsDead(pids []int) {
	h.poll(30*time.Second, "descendant process liveness cleanup", func() bool {
		for _, pid := range pids {
			if err := syscall.Kill(pid, 0); err == nil || !errors.Is(err, syscall.ESRCH) {
				return false
			}
		}
		return true
	})
}

type fixtureQuestionSender struct {
	mu       sync.Mutex
	messages []json.RawMessage
}

func (sender *fixtureQuestionSender) Send(_ context.Context, message json.RawMessage) error {
	sender.mu.Lock()
	defer sender.mu.Unlock()
	sender.messages = append(sender.messages, append(json.RawMessage(nil), message...))
	return nil
}

func unixHTTPClient(socket string) *http.Client {
	return &http.Client{Transport: &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}}
}

func unixJSON(client *http.Client, method, path string, input, output any) error {
	status, data, err := unixRequest(client, method, path, input)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("HTTP %d: %s", status, data)
	}
	if output != nil {
		return json.Unmarshal(data, output)
	}
	return nil
}

func unixRequest(client *http.Client, method, path string, input any) (int, []byte, error) {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return 0, nil, err
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, "http://unix"+path, body)
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("Accept", "application/json")
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	return response.StatusCode, data, err
}

func sameResourceNames(left, right resourceSnapshot) bool {
	return reflect.DeepEqual(sortedKeys(left.Instances), sortedKeys(right.Instances)) && reflect.DeepEqual(sortedKeys(left.Volumes), sortedKeys(right.Volumes))
}

func resourceDiff(before, after resourceSnapshot) string {
	return fmt.Sprintf("instances before=%v after=%v; volumes before=%v after=%v", sortedKeys(before.Instances), sortedKeys(after.Instances), sortedKeys(before.Volumes), sortedKeys(after.Volumes))
}

func sortedKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func kanediasMetadata(values map[string]string) map[string]string {
	metadata := make(map[string]string)
	for key, value := range values {
		if strings.HasPrefix(key, "user.kanedias.") {
			metadata[key] = value
		}
	}
	return metadata
}

func treeSessionIDs(tree supervisor.NodeSnapshot) []string {
	ids := []string{tree.SessionID}
	for _, child := range tree.Children {
		ids = append(ids, treeSessionIDs(child)...)
	}
	return ids
}

func descendantPIDs(root int) []int {
	parents := make(map[int]int)
	entries, _ := os.ReadDir("/proc")
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "status"))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "PPid:") {
				parent, _ := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "PPid:")))
				parents[pid] = parent
				break
			}
		}
	}
	var descendants []int
	frontier := []int{root}
	for len(frontier) > 0 {
		parent := frontier[0]
		frontier = frontier[1:]
		for pid, ppid := range parents {
			if ppid == parent {
				descendants = append(descendants, pid)
				frontier = append(frontier, pid)
			}
		}
	}
	sort.Ints(descendants)
	return descendants
}

func commandOutput(t *testing.T, name string, arguments ...string) string {
	t.Helper()
	command := exec.Command(name, arguments...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v: %s", name, arguments, err, output)
	}
	return strings.TrimSpace(string(output))
}

func canDialTCP(address string) bool {
	connection, err := net.DialTimeout("tcp", address, 250*time.Millisecond)
	if err != nil {
		return false
	}
	_ = connection.Close()
	return true
}

func safeName(value string) string {
	value = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, value)
	if len(value) > 80 {
		value = value[:80]
	}
	return value
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
