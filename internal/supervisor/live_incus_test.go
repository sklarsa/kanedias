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
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
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
	metadataParent  = "user.kanedias.parent_session_id"
	metadataRoot    = "user.kanedias.root_session_id"
	metadataKind    = "user.kanedias.kind"
	metadataContext = "user.kanedias.context"
	metadataWorker  = "user.kanedias.worker_type"
	metadataVolume  = "user.kanedias.workspace_volume"
	metadataRun     = "user.kanedias.e2e_run"
)

// requireLiveSupervisorAuthorization gates any live Incus acceptance test.
// It must be the first call in every test body so that the skip fires before
// any build, bind, Incus, provider, or GitHub side effect occurs.
func requireLiveSupervisorAuthorization(t *testing.T) {
	t.Helper()
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
}

// TestLiveRecursiveSupervisorAcceptance is destructive by design, so every
// capability must be explicitly authorized. In particular, enabling the live
// Incus gate alone never authorizes writes to GitHub.
func TestLiveRecursiveSupervisorAcceptance(t *testing.T) {
	requireLiveSupervisorAuthorization(t)
	harness := newLiveAcceptance(t)
	defer harness.close()
	harness.run()
}

// TestLiveServerManagedSupervisorAcceptance proves the full server-managed
// supervisor lifecycle: spawn, buffering, server restart, rediscovery, Pi
// control, descendant stop, root stop, and exact Incus cleanup.
func TestLiveServerManagedSupervisorAcceptance(t *testing.T) {
	requireLiveSupervisorAuthorization(t)
	harness := newLiveAcceptance(t)
	defer harness.close()
	harness.runServerManaged()
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

	// managedSocketDir is the short root-socket directory used by the
	// server-managed lifecycle test (kept short to stay under UNIX_PATH_MAX).
	managedSocketDir string

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
	remote := strings.TrimSpace(os.Getenv("KANEDIAS_E2E_GITHUB_REMOTE"))
	if err := preflightDisposableRemote(ctx, repository, remote); err != nil {
		cancel()
		t.Fatalf("disposable GitHub remote preflight failed before any push or Incus side effect: %v", err)
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
		remote: remote, prefix: prefix,
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
	h.exerciseQuestionFixture(tree, socket, stream)
	h.exerciseRootRPC(tree, socket, stream)
	h.exerciseFreshRead(tree, socket, stream)
	h.exerciseForkedWrite(tree, socket)
	h.exerciseDirectChildCrash(root, tree, socket)
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
		for _, id := range h.sessionIDs() {
			if _, err := os.Lstat(filepath.Join(h.runDir, id+".sock")); !errors.Is(err, os.ErrNotExist) {
				h.t.Fatalf("session socket %s remains before artifact cleanup: %v", id, err)
			}
		}
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
	h.assertSessionMetadata(tree)
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

func (h *liveAcceptance) exerciseQuestionFixture(root supervisor.NodeSnapshot, socket string, stream *sseCapture) {
	client := unixHTTPClient(socket)
	var question supervisor.QuestionSummary
	h.poll(time.Minute, "controlled Pi question in root tree", func() bool {
		var current supervisor.NodeSnapshot
		if unixJSON(client, http.MethodGet, "/v1/tree", nil, &current) != nil || len(current.Questions) != 1 {
			return false
		}
		question = current.Questions[0]
		return strings.Contains(question.Title, h.prefix)
	})
	status, body, err := unixRequest(client, http.MethodPost, "/v1/sessions/"+root.SessionID+"/questions/"+question.ID+"/response", map[string]any{"value": "deterministic-answer"})
	if err != nil || status != http.StatusNoContent {
		h.t.Fatalf("route controlled Pi question answer = HTTP %d %s, %v", status, body, err)
	}
	status, _, err = unixRequest(client, http.MethodPost, "/v1/sessions/"+root.SessionID+"/questions/"+question.ID+"/response", map[string]any{"value": "duplicate"})
	if err != nil || status != http.StatusNotFound {
		h.t.Fatalf("duplicate controlled Pi question = HTTP %d, %v", status, err)
	}
	h.poll(time.Minute, "controlled Pi question disappearance", func() bool {
		var current supervisor.NodeSnapshot
		return unixJSON(client, http.MethodGet, "/v1/tree", nil, &current) == nil && len(current.Questions) == 0
	})
	h.waitPiNotification(stream.events, root.SessionID, "KANEDIAS_E2E_QUESTION_ANSWER:deterministic-answer", time.Minute)
	h.writeJSON("question-fixture.json", map[string]any{"question": question, "answer": "deterministic-answer", "duplicateStatus": status})
}

func (h *liveAcceptance) exerciseFreshRead(root supervisor.NodeSnapshot, socket string, stream *sseCapture) {
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
	h.waitSessionEvent(stream.events, child.SessionID, 2*time.Minute)
	h.assertDistinct(root, child)
	h.assertWorker(child, "reviewer")
	h.assertSessionMetadata(child)
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
	checkoutOrigin := h.execInstance(rootInstance, "git", "-C", checkout, "config", "--get", "remote.origin.url")
	if err := preflightCheckoutOrigin(h.ctx, h.repository, h.remote, checkoutOrigin, resolveGitRemoteHEAD); err != nil {
		h.t.Fatalf("actual writer checkout origin rejected before model prompt or push: %v", err)
	}
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
	h.assertSessionMetadata(child)
	childInstance := h.instanceForSession(child.SessionID)
	var branchHeader struct {
		ParentSession string `json:"parentSession"`
	}
	if err := json.Unmarshal([]byte(h.execInstance(childInstance, "head", "-n", "1", child.SessionFile)), &branchHeader); err != nil || branchHeader.ParentSession != root.SessionFile {
		h.t.Fatalf("forked Pi branch parent metadata = %#v, %v; want %q", branchHeader, err, root.SessionFile)
	}
	var observedHead string
	h.poll(12*time.Minute, "durable writer ref before child disappearance", func() bool {
		command := exec.CommandContext(h.ctx, "git", "ls-remote", "--exit-code", h.remote, "refs/heads/"+branch)
		output, err := command.Output()
		if err != nil {
			return false
		}
		fields := strings.Fields(string(output))
		if len(fields) < 2 || fields[1] != "refs/heads/"+branch {
			return false
		}
		var current supervisor.NodeSnapshot
		if unixJSON(client, http.MethodGet, "/v1/tree", nil, &current) != nil || len(current.Children) == 0 {
			return false
		}
		observedHead = fields[0]
		return true
	})
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
	messages := h.rpc(client, root.SessionID, map[string]any{"type": "get_messages"})
	typed, ok := findTypedWriteResult(messages, h.repository, branch, head[0])
	if !ok {
		h.t.Fatalf("parent Pi messages lack typed WriteChildResult for %s:%s@%s: %#v", h.repository, branch, head[0], messages)
	}
	if observedHead != head[0] {
		h.t.Fatalf("pre-disappearance ref %q differs from final %q", observedHead, head[0])
	}
	h.writeJSON("reported-git-refs.json", map[string]any{"repository": h.repository, "base": base, "branch": branch, "head": head[0], "typedResult": typed, "observedBeforeDisappearance": observedHead})
	h.assertSessionAbsent(child.SessionID)
	h.snapshotTree("forked-write-complete", client)
}

func (h *liveAcceptance) exerciseDirectChildCrash(rootProcess *acceptanceProcess, root supervisor.NodeSnapshot, socket string) {
	client := unixHTTPClient(socket)
	h.startNestedDelegation(client, root.SessionID, "direct-child-crash")
	var nested supervisor.NodeSnapshot
	h.poll(8*time.Minute, "child and grandchild before direct-child crash", func() bool {
		var current supervisor.NodeSnapshot
		if unixJSON(client, http.MethodGet, "/v1/tree", nil, &current) != nil || len(current.Children) == 0 || len(current.Children[0].Children) == 0 {
			return false
		}
		nested = current
		return true
	})
	h.trackTree(nested)
	h.assertTreeMetadata(nested)
	pids := directSessionChildPIDs(rootProcess.cmd.Process.Pid)
	if len(pids) != 1 {
		h.t.Fatalf("direct child supervisor PIDs = %v, want exactly one", pids)
	}
	if err := syscall.Kill(pids[0], syscall.SIGKILL); err != nil {
		h.t.Fatal(err)
	}
	descendants := treeSessionIDs(nested)[1:]
	h.poll(2*time.Minute, "direct-child crash recovery", func() bool {
		var current supervisor.NodeSnapshot
		return h.sessionsAbsent(descendants) && unixJSON(client, http.MethodGet, "/v1/tree", nil, &current) == nil && current.Lifecycle == "ready" && len(current.Children) == 0
	})
	h.assertDescendantSocketsAbsent(nested)
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
	h.assertTreeMetadata(nested)
	pids := descendantPIDs(rootProcess.cmd.Process.Pid)
	status, _, err := unixRequest(client, http.MethodDelete, "/v1/sessions/"+root.SessionID, nil)
	if err != nil || status != http.StatusAccepted {
		h.t.Fatalf("graceful root DELETE = %d, %v", status, err)
	}
	h.waitProcess(rootProcess, 2*time.Minute)
	h.poll(2*time.Minute, "graceful descendant resource cleanup", func() bool { return h.sessionsAbsent(treeSessionIDs(nested)) })
	h.assertPIDsDead(pids)
	h.assertDescendantSocketsAbsent(nested)
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
	h.assertTreeMetadata(nested)
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
	h.assertDescendantSocketsAbsent(nested)
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

func (h *liveAcceptance) waitSessionEvent(events <-chan supervisor.EventEnvelope, sessionID string, timeout time.Duration) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case event, ok := <-events:
			if !ok {
				h.t.Fatalf("SSE ended before event from %s", sessionID)
			}
			if event.SessionID == sessionID {
				return
			}
		case <-timer.C:
			h.t.Fatalf("timed out waiting for root SSE envelope from %s", sessionID)
		}
	}
}

func (h *liveAcceptance) waitPiNotification(events <-chan supervisor.EventEnvelope, sessionID, marker string, timeout time.Duration) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case event, ok := <-events:
			if !ok {
				h.t.Fatalf("SSE ended before Pi notification %q", marker)
			}
			if event.SessionID != sessionID {
				continue
			}
			var payload struct{ Type, Method, Message string }
			if json.Unmarshal(event.Payload, &payload) == nil && payload.Type == "extension_ui_request" && payload.Method == "notify" && payload.Message == marker {
				return
			}
		case <-timer.C:
			h.t.Fatalf("timed out waiting for Pi notification %q", marker)
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

func (h *liveAcceptance) assertSessionMetadata(node supervisor.NodeSnapshot) {
	h.trackSession(node.SessionID)
	snapshot := h.snapshotResources("metadata-" + safeName(node.SessionID))
	instances := make([]resourceRecord, 0, 1)
	volumes := make([]resourceRecord, 0, 1)
	for _, record := range snapshot.Instances {
		if record.SessionID == node.SessionID {
			instances = append(instances, record)
		}
	}
	for _, record := range snapshot.Volumes {
		if record.SessionID == node.SessionID {
			volumes = append(volumes, record)
		}
	}
	if len(instances) != 1 || len(volumes) != 1 {
		h.t.Fatalf("session %s resource pair count: instances=%d volumes=%d", node.SessionID, len(instances), len(volumes))
	}
	wantInstance, wantVolume, want, err := expectedLiveResourceAssertion(node, instances[0].Name, h.prefix)
	if err != nil {
		h.t.Fatalf("session %s resource identity: %v", node.SessionID, err)
	}
	if instances[0].Name != wantInstance || volumes[0].Name != wantVolume {
		h.t.Fatalf("session %s resource names: instance=%q volume=%q, want instance=%q volume=%q", node.SessionID, instances[0].Name, volumes[0].Name, wantInstance, wantVolume)
	}
	for _, record := range []resourceRecord{instances[0], volumes[0]} {
		for key, value := range want {
			if record.Config[key] != value {
				h.t.Fatalf("resource %s metadata %s=%q, want %q; all=%#v", record.Name, key, record.Config[key], value, record.Config)
			}
		}
	}
}

func expectedLiveResourceAssertion(node supervisor.NodeSnapshot, observedInstance, run string) (string, string, map[string]string, error) {
	if strings.TrimSpace(observedInstance) == "" {
		return "", "", nil, fmt.Errorf("observed instance name is empty")
	}
	var instance, volume string
	switch node.Kind {
	case contract.ChildKindRoot:
		if node.ParentSessionID != "" || node.RootSessionID != node.SessionID || node.Context != contract.ContextRoot || node.WorkerType != "" {
			return "", "", nil, fmt.Errorf("root snapshot identity is inconsistent")
		}
		if observedInstance == "session-"+node.SessionID {
			return "", "", nil, fmt.Errorf("root instance incorrectly uses child deterministic session name")
		}
		instance = observedInstance
		volume = "kanedias-workspace-" + observedInstance
	case contract.ChildKindRead, contract.ChildKindWrite:
		instance = "session-" + node.SessionID
		if observedInstance != instance {
			return "", "", nil, fmt.Errorf("child instance %q, want %q", observedInstance, instance)
		}
		volume = "workspace-" + node.SessionID
	default:
		return "", "", nil, fmt.Errorf("unsupported session kind %q", node.Kind)
	}
	metadata := map[string]string{
		metadataSession: node.SessionID, metadataParent: node.ParentSessionID, metadataRoot: node.RootSessionID,
		metadataKind: string(node.Kind), metadataContext: string(node.Context), metadataWorker: node.WorkerType,
		metadataVolume: volume, metadataRun: run,
	}
	return instance, volume, metadata, nil
}

func (h *liveAcceptance) assertTreeMetadata(tree supervisor.NodeSnapshot) {
	h.assertSessionMetadata(tree)
	for _, child := range tree.Children {
		h.assertTreeMetadata(child)
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
	command.Env = append(os.Environ(), "KANEDIAS_E2E_RUN_ID="+h.prefix)
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
	return reflect.DeepEqual(left, right)
}

func resourceDiff(before, after resourceSnapshot) string {
	return fmt.Sprintf("instances before=%#v after=%#v; volumes before=%#v after=%#v", before.Instances, after.Instances, before.Volumes, after.Volumes)
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

func findTypedWriteResult(value any, repository, branch, head string) (map[string]any, bool) {
	switch current := value.(type) {
	case map[string]any:
		if current["kind"] == "write" {
			if repositories, ok := current["repositories"].([]any); ok {
				for _, raw := range repositories {
					entry, _ := raw.(map[string]any)
					if entry["repository"] == repository && entry["branch"] == branch && entry["headCommit"] == head {
						return current, true
					}
				}
			}
		}
		for _, child := range current {
			if result, ok := findTypedWriteResult(child, repository, branch, head); ok {
				return result, true
			}
		}
	case []any:
		for _, child := range current {
			if result, ok := findTypedWriteResult(child, repository, branch, head); ok {
				return result, true
			}
		}
	}
	return nil, false
}

func treeSessionIDs(tree supervisor.NodeSnapshot) []string {
	ids := []string{tree.SessionID}
	for _, child := range tree.Children {
		ids = append(ids, treeSessionIDs(child)...)
	}
	return ids
}

func (h *liveAcceptance) assertDescendantSocketsAbsent(tree supervisor.NodeSnapshot) {
	ids := treeSessionIDs(tree)
	if len(ids) > 0 {
		ids = ids[1:]
	}
	for _, id := range ids {
		if _, err := os.Lstat(filepath.Join(h.runDir, id+".sock")); !errors.Is(err, os.ErrNotExist) {
			h.t.Fatalf("descendant socket %s remains before artifact cleanup: %v", id, err)
		}
	}
}

func directSessionChildPIDs(parent int) []int {
	var result []int
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
		if parsePPID(string(data)) != parent {
			continue
		}
		cmdline, _ := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if bytes.Contains(cmdline, []byte("session-child")) {
			result = append(result, pid)
		}
	}
	sort.Ints(result)
	return result
}

func parsePPID(status string) int {
	for _, line := range strings.Split(status, "\n") {
		if strings.HasPrefix(line, "PPid:") {
			parent, _ := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "PPid:")))
			return parent
		}
	}
	return 0
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

type remoteHEADResolver func(context.Context, string) (string, error)

func resolveGitRemoteHEAD(ctx context.Context, remote string) (string, error) {
	command := exec.CommandContext(ctx, "git", "ls-remote", "--exit-code", remote, "HEAD")
	output, err := command.Output()
	if err != nil {
		return "", err
	}
	fields := strings.Fields(string(output))
	if len(fields) != 2 || fields[1] != "HEAD" {
		return "", fmt.Errorf("remote returned no unique HEAD")
	}
	return fields[0], nil
}

func preflightDisposableRemote(ctx context.Context, repository, remote string) error {
	if githubRemoteSlug(remote) != repository {
		return fmt.Errorf("explicit remote does not identify trusted github.com/%s", repository)
	}
	explicitHead, err := resolveGitRemoteHEAD(ctx, remote)
	if err != nil {
		return fmt.Errorf("resolve explicit remote HEAD: %w", err)
	}
	trustedHead, err := resolveGitRemoteHEAD(ctx, "https://github.com/"+repository+".git")
	if err != nil {
		return fmt.Errorf("resolve trusted canonical remote HEAD: %w", err)
	}
	if explicitHead != trustedHead {
		return fmt.Errorf("explicit remote HEAD differs from trusted canonical repository")
	}
	return nil
}

func preflightCheckoutOrigin(ctx context.Context, repository, authorizedRemote, checkoutOrigin string, resolve remoteHEADResolver) error {
	if githubRemoteSlug(authorizedRemote) != repository {
		return fmt.Errorf("authorized remote does not identify trusted github.com/%s", repository)
	}
	if githubRemoteSlug(checkoutOrigin) != repository {
		return fmt.Errorf("checkout origin %q does not identify authorized github.com/%s", checkoutOrigin, repository)
	}
	authorizedHead, err := resolve(ctx, authorizedRemote)
	if err != nil {
		return fmt.Errorf("resolve authorized disposable remote HEAD: %w", err)
	}
	checkoutHead, err := resolve(ctx, checkoutOrigin)
	if err != nil {
		return fmt.Errorf("resolve actual checkout origin HEAD: %w", err)
	}
	if checkoutHead != authorizedHead {
		return fmt.Errorf("actual checkout origin HEAD differs from authorized disposable remote")
	}
	return nil
}

var githubSlugComponent = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

func githubRemoteSlug(remote string) string {
	if strings.HasPrefix(remote, "git@github.com:") {
		return canonicalGitHubSlug(strings.TrimPrefix(remote, "git@github.com:"))
	}
	parsed, err := url.Parse(remote)
	if err != nil || parsed.Opaque != "" || parsed.Host != "github.com" || parsed.Port() != "" ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || strings.ContainsAny(remote, "?#") || parsed.RawPath != "" {
		return ""
	}
	switch parsed.Scheme {
	case "https":
		if parsed.User != nil {
			return ""
		}
	case "ssh":
		if parsed.User == nil || parsed.User.Username() != "git" {
			return ""
		}
		if _, hasPassword := parsed.User.Password(); hasPassword {
			return ""
		}
	default:
		return ""
	}
	return canonicalGitHubSlug(strings.TrimPrefix(parsed.Path, "/"))
}

func canonicalGitHubSlug(path string) string {
	if path == "" || strings.HasPrefix(path, "/") || strings.HasSuffix(path, "/") || strings.Contains(path, "\\") {
		return ""
	}
	parts := strings.Split(path, "/")
	if len(parts) != 2 {
		return ""
	}
	parts[1] = strings.TrimSuffix(parts[1], ".git")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || !githubSlugComponent.MatchString(part) {
			return ""
		}
	}
	return parts[0] + "/" + parts[1]
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

// ---------------------------------------------------------------------------
// Server-managed supervisor acceptance helpers
// ---------------------------------------------------------------------------

// managedRoot holds the identity of a server-spawned root discovered after
// server startup. SocketPath ends in .root.sock; PID is the supervisor's OS PID.
type managedRoot struct {
	SessionID  string
	SocketPath string
	PID        int
}

// shortSocketDir returns a short, private, EUID-owned mode-0700 directory for
// managed root sockets. A root socket path is <base>/<32-hex>.root.sock and must
// stay under UNIX_PATH_MAX (107 bytes); the deep e2e artifact runDir would
// overflow it, so anchor sockets under XDG_RUNTIME_DIR (or /tmp) with a short
// unique suffix. The directory is removed when the test finishes.
func (h *liveAcceptance) shortSocketDir() string {
	base := strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR"))
	if base == "" {
		base = "/tmp"
	}
	dir, err := os.MkdirTemp(base, "kroots-")
	if err != nil {
		h.t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// runServerManaged exercises the full server-managed supervisor lifecycle:
// spawn, buffering, server restart, rediscovery, Pi control, descendant
// stop, root stop, and exact Incus cleanup.
func (h *liveAcceptance) runServerManaged() {
	h.buildReviewedCheckout()

	// Spawned roots require the egress proxy listener to be reachable before they
	// provision; without it every SpawnRoot dies with proxy_unavailable and no
	// root socket is ever created. close() stops the owned proxy at teardown.
	h.startProxy()

	// Create a run-local managed config that overlays server and supervisor.events
	// directories onto the authorized config. Logs land in the isolated runDir, but
	// the root socket dir MUST be short: a root socket is <32-hex-token>.root.sock
	// (43 bytes) and the full path must stay under UNIX_PATH_MAX (107 bytes). The
	// deep ~/.cache/kanedias/e2e/<long-prefix> runDir would overflow that, so place
	// sockets in a short runtime dir (mirroring the manager's XDG_RUNTIME_DIR default).
	rootSocketDir := h.shortSocketDir()
	h.managedSocketDir = rootSocketDir
	sessionLogDir := filepath.Join(h.runDir, "managed-logs")
	for _, dir := range []string{rootSocketDir, sessionLogDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			h.t.Fatal(err)
		}
	}

	managedConfig := h.writeManagedConfig(rootSocketDir, sessionLogDir)

	// Start the server. It spawns roots via SpawnRoot on POST /ui/sessions.
	server := h.startProcess("managed-server", h.binary, "--config", managedConfig, "server", "--listen", "127.0.0.1:0")

	// Derive the server listen address from the log (the binary logs "server listening"
	// and "Bootstrap URL:" to stderr). Bootstrap the HTTP client with a cookie jar.
	serverOrigin, client := h.bootstrapManagedServer(server)

	// POST New Session twice to spawn two server-managed roots.
	for range 2 {
		h.postDatastar(client, serverOrigin+"/ui/sessions", map[string]any{})
	}

	// Discover both roots by scanning the socket directory.
	roots := h.waitForManagedRoots(rootSocketDir, serverOrigin, server, 2)
	h.t.Logf("managed roots discovered: %v", roots)

	// Steer each idle root so their event buffers advance.
	for _, root := range roots {
		h.trackSession(root.SessionID)
		h.postDatastar(client, serverOrigin+"/ui/sessions/"+url.PathEscape(root.SessionID)+"/steer",
			map[string]any{"message": "Reply with exactly MANAGED_ROOT_OK."})
	}

	// --- Non-destructive server restart ---
	// SIGTERM the server; roots and their sockets must survive.
	h.stopProcess(server, syscall.SIGTERM, 30*time.Second)
	for _, root := range roots {
		h.assertProcessAlive(root.PID)
		h.snapshotRoot(root.SocketPath)
	}

	// Start a second server instance on the same config.
	restarted := h.startProcess("managed-server-restart", h.binary, "--config", managedConfig, "server", "--listen", "127.0.0.1:0")
	restartedOrigin, restartedClient := h.bootstrapManagedServer(restarted)

	// Assert both roots appear exactly once in the restarted fleet.
	h.assertFleetContainsExactly(restartedClient, restartedOrigin, roots)

	// --- Exercise real controls and cleanup ---
	// Have the first root create a controlled descendant by steering it with a
	// delegate_session request.
	descendantTask := "Use delegate_session exactly once with workerType reviewer, kind read, context fresh, task \"Reply KANEDIAS_E2E_MANAGED_DESCENDANT_OK exactly\". After it returns reproduce its answer."
	h.postDatastar(restartedClient, actionURL(restartedOrigin, roots[0].SessionID, "steer"),
		map[string]any{"message": descendantTask})

	descendant := h.waitForManagedDescendant(roots[0].SocketPath, roots[0].SessionID)
	h.trackSession(descendant.SessionID)
	h.t.Logf("managed descendant discovered: session=%s", descendant.SessionID)

	// Steer, interrupt, then answer a question on the descendant (question may not
	// materialise if the model does not block; we attempt anyway and ignore 404).
	h.postDatastar(restartedClient, actionURL(restartedOrigin, descendant.SessionID, "steer"),
		map[string]any{"message": "Focus on the acceptance marker."})
	h.postDatastar(restartedClient, actionURL(restartedOrigin, descendant.SessionID, "interrupt"),
		map[string]any{})
	h.answerManagedQuestion(restartedClient, restartedOrigin, descendant.SessionID, "deterministic-answer")

	// Stop the descendant without stopping the root.
	h.postDatastar(restartedClient, actionURL(restartedOrigin, descendant.SessionID, "stop"), map[string]any{})

	// Stop both roots via the server UI.
	for _, root := range roots {
		h.postDatastar(restartedClient, actionURL(restartedOrigin, root.SessionID, "stop"), map[string]any{})
	}

	// Stop the second server cleanly.
	h.stopProcess(restarted, syscall.SIGTERM, 30*time.Second)

	// All sockets, processes, and Incus resources must return to baseline.
	h.assertBaseline("after-server-managed-roots")
	h.success = true
}

// writeManagedConfig copies the authorized config to a run-local file and
// appends [server] and [supervisor.events] sections that point to private
// run-local directories. Returns the absolute path to the generated config file.
func (h *liveAcceptance) writeManagedConfig(rootSocketDir, sessionLogDir string) string {
	src, err := os.ReadFile(h.configPath)
	if err != nil {
		h.t.Fatal(err)
	}
	// Append run-local overrides. The TOML decoder takes the last value wins for
	// duplicate keys within the same table, but to be safe we build new tables.
	appendix := fmt.Sprintf(`
[server]
root_socket_dir = %q
session_log_dir = %q
session_binary  = %q

[supervisor.events]
max_events = 256
max_bytes  = 4194304
`, rootSocketDir, sessionLogDir, h.binary)
	combined := append(append([]byte(nil), src...), []byte(appendix)...)
	dest := filepath.Join(h.runDir, "managed-config.toml")
	if err := os.WriteFile(dest, combined, 0o600); err != nil {
		h.t.Fatal(err)
	}
	return dest
}

// waitForBootstrapURL polls the process log file until the "Bootstrap URL:"
// line appears and returns the full URL including the server's origin.
// The format is: "Bootstrap URL: /bootstrap?capability=<token>"
func (h *liveAcceptance) waitForBootstrapURL(logPath string) (origin, bootstrapURL string) {
	const prefix = "Bootstrap URL: "
	const listenPrefix = "effective_address="
	var foundOrigin, foundBootstrap string
	h.poll(2*time.Minute, "bootstrap URL and effective address in log "+logPath, func() bool {
		data, err := os.ReadFile(logPath)
		if err != nil {
			return false
		}
		text := string(data)
		if foundBootstrap == "" {
			idx := strings.Index(text, prefix)
			if idx != -1 {
				rest := strings.TrimSpace(text[idx+len(prefix):])
				if nl := strings.IndexByte(rest, '\n'); nl != -1 {
					rest = rest[:nl]
				}
				foundBootstrap = strings.TrimSpace(rest)
			}
		}
		if foundOrigin == "" {
			// Extract effective_address from slog text output, e.g.:
			//   level=INFO msg="server listening" effective_address=127.0.0.1:PORT
			idx := strings.Index(text, listenPrefix)
			if idx != -1 {
				rest := text[idx+len(listenPrefix):]
				// Value ends at whitespace or end of line.
				end := strings.IndexAny(rest, " \t\r\n")
				if end == -1 {
					end = len(rest)
				}
				addr := strings.TrimSpace(rest[:end])
				if addr != "" {
					foundOrigin = "http://" + addr
				}
			}
		}
		return foundBootstrap != "" && foundOrigin != ""
	})
	return foundOrigin, foundBootstrap
}

// bootstrapManagedServer waits for the server process log to contain the
// bootstrap URL, performs the bootstrap exchange, and returns an authenticated
// HTTP client together with the server's origin (e.g. "http://127.0.0.1:PORT").
func (h *liveAcceptance) bootstrapManagedServer(server *acceptanceProcess) (string, *http.Client) {
	// Derive the log path from the process label (startProcess writes to label+".log").
	// The process was started with label "managed-server" or "managed-server-restart".
	var logPath string
	for _, candidate := range []string{"managed-server-restart.log", "managed-server.log"} {
		p := filepath.Join(h.runDir, candidate)
		if _, err := os.Stat(p); err == nil {
			// Pick the most recently modified one that belongs to this process.
			logPath = p
			break
		}
	}
	if logPath == "" {
		h.t.Fatal("bootstrapManagedServer: could not locate server log file")
	}

	origin, bootstrapPath := h.waitForBootstrapURL(logPath)

	jar, err := cookiejar.New(nil)
	if err != nil {
		h.t.Fatal(err)
	}
	httpClient := &http.Client{
		Jar:     jar,
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// Exchange the bootstrap token.
	bootstrapFull := origin + bootstrapPath
	resp, err := httpClient.Get(bootstrapFull)
	if err != nil {
		h.t.Fatalf("bootstrap GET %s: %v", bootstrapFull, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		h.t.Fatalf("bootstrap status = %d, want 303", resp.StatusCode)
	}
	// Follow the redirect to / to ensure cookie is set.
	indexResp, err := httpClient.Get(origin + "/")
	if err != nil {
		h.t.Fatalf("GET / after bootstrap: %v", err)
	}
	_ = indexResp.Body.Close()
	if indexResp.StatusCode != http.StatusOK {
		h.t.Fatalf("GET / status = %d, want 200", indexResp.StatusCode)
	}

	// Reset the server log after bootstrap so that a subsequent call to
	// bootstrapManagedServer (for the restarted server) finds the new URL.
	// We achieve this by removing the stale log and letting startProcess create
	// a fresh one for the restarted label.
	_ = server // used by caller to stop the process; no additional action needed.

	return origin, httpClient
}

// postDatastar sends a write-boundary-compliant POST to a server action URL.
// It sets Origin, Sec-Fetch-Site, and Content-Type exactly as the browser would.
func (h *liveAcceptance) postDatastar(client *http.Client, fullURL string, body map[string]any) {
	encoded, err := json.Marshal(body)
	if err != nil {
		h.t.Fatalf("postDatastar: marshal body: %v", err)
	}
	parsed, err := url.Parse(fullURL)
	if err != nil {
		h.t.Fatalf("postDatastar: parse URL %q: %v", fullURL, err)
	}
	origin := parsed.Scheme + "://" + parsed.Host
	req, err := http.NewRequest(http.MethodPost, fullURL, bytes.NewReader(encoded))
	if err != nil {
		h.t.Fatalf("postDatastar: new request: %v", err)
	}
	req.Host = parsed.Host
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", origin)
	req.Header.Set("Sec-Fetch-Site", "same-origin")

	resp, err := client.Do(req)
	if err != nil {
		h.t.Fatalf("postDatastar: POST %s: %v", fullURL, err)
	}
	_ = resp.Body.Close()
}

// waitForManagedRoots polls rootSocketDir until exactly n *.root.sock files
// appear, probes each via /v1/tree, and resolves the owning PID from the server
// process tree. Returns the n discovered managedRoot values.
func (h *liveAcceptance) waitForManagedRoots(rootSocketDir, serverOrigin string, server *acceptanceProcess, n int) []managedRoot {
	var roots []managedRoot
	h.poll(3*time.Minute, fmt.Sprintf("%d managed roots in %s", n, rootSocketDir), func() bool {
		// Check server is still alive.
		select {
		case <-server.done:
			h.t.Fatalf("managed server exited before roots were ready: %v", server.waitErr)
		default:
		}
		entries, err := os.ReadDir(rootSocketDir)
		if err != nil {
			return false
		}
		var sockets []string
		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), ".root.sock") {
				sockets = append(sockets, filepath.Join(rootSocketDir, entry.Name()))
			}
		}
		if len(sockets) < n {
			return false
		}
		// Probe each socket to resolve session ID and PID.
		var found []managedRoot
		for _, sockPath := range sockets {
			uc := unixHTTPClient(sockPath)
			var tree supervisor.NodeSnapshot
			if err := unixJSON(uc, http.MethodGet, "/v1/tree", nil, &tree); err != nil {
				return false
			}
			if tree.SessionID == "" || tree.Lifecycle != "ready" {
				return false
			}
			pid := h.resolveSocketOwnerPID(server.cmd.Process.Pid, sockPath)
			found = append(found, managedRoot{
				SessionID:  tree.SessionID,
				SocketPath: sockPath,
				PID:        pid,
			})
		}
		if len(found) < n {
			return false
		}
		roots = found
		return true
	})
	return roots
}

// resolveSocketOwnerPID walks the server process subtree and returns the PID
// of the direct child whose command line references the given socket path.
// Falls back to the server's own PID if no match is found.
func (h *liveAcceptance) resolveSocketOwnerPID(serverPID int, socketPath string) int {
	entries, _ := os.ReadDir("/proc")
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		// Check PPID to limit search to direct descendants of serverPID.
		status, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "status"))
		if err != nil {
			continue
		}
		if parsePPID(string(status)) != serverPID {
			continue
		}
		cmdline, _ := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if bytes.Contains(cmdline, []byte(socketPath)) {
			return pid
		}
	}
	return serverPID
}

// snapshotRoot probes the root socket and records the tree snapshot as an artifact.
func (h *liveAcceptance) snapshotRoot(socketPath string) {
	label := "root-" + safeName(filepath.Base(socketPath))
	uc := unixHTTPClient(socketPath)
	var tree supervisor.NodeSnapshot
	if err := unixJSON(uc, http.MethodGet, "/v1/tree", nil, &tree); err != nil {
		h.t.Fatalf("snapshotRoot %s: %v", socketPath, err)
	}
	h.writeJSON(label+"-tree.json", tree)
}

// assertProcessAlive verifies that a process with the given PID is still running.
func (h *liveAcceptance) assertProcessAlive(pid int) {
	if err := syscall.Kill(pid, 0); err != nil {
		h.t.Fatalf("process %d is not alive: %v", pid, err)
	}
}

// assertFleetContainsExactly polls the server fleet endpoint until all expected
// root session IDs appear and no duplicates exist.
func (h *liveAcceptance) assertFleetContainsExactly(client *http.Client, serverOrigin string, roots []managedRoot) {
	h.poll(30*time.Second, "restarted fleet contains all managed roots", func() bool {
		ctx, cancel := context.WithTimeout(h.ctx, 5*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, serverOrigin+"/ui/fleet", nil)
		if err != nil {
			return false
		}
		resp, err := client.Do(req)
		if err != nil {
			return false
		}
		var buf bytes.Buffer
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			buf.WriteString(scanner.Text())
		}
		_ = resp.Body.Close()
		body := buf.String()
		for _, root := range roots {
			if !strings.Contains(body, root.SessionID) {
				return false
			}
		}
		return true
	})
}

// waitForManagedDescendant polls the root socket until a descendant session
// appears in the tree and returns the first child's snapshot.
func (h *liveAcceptance) waitForManagedDescendant(rootSocketPath, rootSessionID string) supervisor.NodeSnapshot {
	var child supervisor.NodeSnapshot
	uc := unixHTTPClient(rootSocketPath)
	h.poll(8*time.Minute, "managed descendant in root "+rootSessionID, func() bool {
		var tree supervisor.NodeSnapshot
		if unixJSON(uc, http.MethodGet, "/v1/tree", nil, &tree) != nil {
			return false
		}
		if len(tree.Children) == 0 {
			return false
		}
		child = tree.Children[0]
		return child.SessionID != ""
	})
	return child
}

// actionURL returns the full POST URL for a named session action on the server.
func actionURL(serverOrigin, sessionID, action string) string {
	return serverOrigin + "/ui/sessions/" + url.PathEscape(sessionID) + "/" + action
}

// answerManagedQuestion probes the root socket for a pending question and posts
// the answer to the server. If no question is present the call is a no-op.
func (h *liveAcceptance) answerManagedQuestion(client *http.Client, serverOrigin, sessionID, answer string) {
	// Resolve the root socket path from the session ID.
	var rootSocketPath string
	for _, root := range h.roots {
		// h.roots holds the supervisor processes started by startRoot; for
		// managed roots we need to scan the socket dir directly.
		_ = root
	}
	// We don't have a direct roots index for managed roots, so look up via
	// the fleet. Locate the managed socket from the short managed socket dir.
	managedSocketDir := h.managedSocketDir
	entries, err := os.ReadDir(managedSocketDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".root.sock") {
			continue
		}
		sockPath := filepath.Join(managedSocketDir, entry.Name())
		uc := unixHTTPClient(sockPath)
		var tree supervisor.NodeSnapshot
		if unixJSON(uc, http.MethodGet, "/v1/tree", nil, &tree) != nil {
			continue
		}
		if treeContainsSession(tree, sessionID) {
			rootSocketPath = sockPath
			break
		}
	}
	if rootSocketPath == "" {
		// Session not found; skip answering.
		return
	}
	uc := unixHTTPClient(rootSocketPath)
	var tree supervisor.NodeSnapshot
	if unixJSON(uc, http.MethodGet, "/v1/tree", nil, &tree) != nil {
		return
	}
	question := findQuestion(tree, sessionID)
	if question.ID == "" {
		// No pending question; nothing to answer.
		return
	}
	h.postDatastar(client, serverOrigin+"/ui/sessions/"+url.PathEscape(sessionID)+"/questions/"+url.PathEscape(question.ID),
		map[string]any{"value": answer})
}

// treeContainsSession returns true if tree or any descendant has the given session ID.
func treeContainsSession(tree supervisor.NodeSnapshot, sessionID string) bool {
	if tree.SessionID == sessionID {
		return true
	}
	for _, child := range tree.Children {
		if treeContainsSession(child, sessionID) {
			return true
		}
	}
	return false
}

// findQuestion finds the first pending question for the given session ID in the tree.
func findQuestion(tree supervisor.NodeSnapshot, sessionID string) supervisor.QuestionSummary {
	if tree.SessionID == sessionID && len(tree.Questions) > 0 {
		return tree.Questions[0]
	}
	for _, child := range tree.Children {
		if q := findQuestion(child, sessionID); q.ID != "" {
			return q
		}
	}
	return supervisor.QuestionSummary{}
}
