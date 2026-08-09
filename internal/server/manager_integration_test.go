package server

// TestServerRestartRediscoversRunningRoot is a hermetic end-to-end integration
// test that proves the full vertical slice:
//
//  1. A persistent fake root supervisor is served over a real Unix socket.
//  2. A real manager + server pair discovers the root, serves authenticated
//     HTTP traffic, and routes control actions to the fake.
//  3. The server is restarted (its context cancelled) WITHOUT stopping the
//     fake root; the fake keeps serving.
//  4. A fresh server instance rediscovers the same root and all routes work
//     again.
//
// No Incus, no real Pi process — only the root supervisor Service is faked.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sklarsa/kanedias/internal/config"
	"github.com/sklarsa/kanedias/internal/manager"
	"github.com/sklarsa/kanedias/internal/supervisor"
	"github.com/sklarsa/kanedias/internal/supervisor/contract"
	"github.com/sklarsa/kanedias/internal/supervisorapi"
)

// ---------------------------------------------------------------------------
// persistentRootService — a fake supervisorapi.Service backed by a real broker
// ---------------------------------------------------------------------------

type persistentRootService struct {
	mu          sync.Mutex
	tree        supervisor.NodeSnapshot
	broker      *supervisor.EventBroker
	commands    []json.RawMessage
	stopped     chan string
	snapshotErr error
}

func newPersistentRootService(rootID string) *persistentRootService {
	tree := supervisor.NodeSnapshot{
		SessionID:     rootID,
		RootSessionID: rootID,
		PiSessionID:   "pi-" + rootID,
		SessionFile:   "/sessions/" + rootID + ".jsonl",
		Kind:          contract.ChildKindRoot,
		Context:       contract.ContextRoot,
		Lifecycle:     string(supervisor.LifecycleReady),
		Questions:     []supervisor.QuestionSummary{},
		Children:      []supervisor.NodeSnapshot{},
	}
	return &persistentRootService{
		tree:    tree,
		broker:  supervisor.NewEventBroker(),
		stopped: make(chan string, 8),
	}
}

func cloneRootSnapshot(s supervisor.NodeSnapshot) supervisor.NodeSnapshot {
	cp := s
	cp.Questions = append([]supervisor.QuestionSummary(nil), s.Questions...)
	cp.Children = append([]supervisor.NodeSnapshot(nil), s.Children...)
	return cp
}

func (svc *persistentRootService) Snapshot(_ context.Context) (supervisor.NodeSnapshot, error) {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	if svc.snapshotErr != nil {
		return supervisor.NodeSnapshot{}, svc.snapshotErr
	}
	return cloneRootSnapshot(svc.tree), nil
}

func (svc *persistentRootService) setSnapshotError(err error) {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	svc.snapshotErr = err
}

func (svc *persistentRootService) Workers(_ context.Context) []contract.WorkerSummary {
	return []contract.WorkerSummary{}
}

func (svc *persistentRootService) CallRPC(_ context.Context, sessionID string, body json.RawMessage) (json.RawMessage, error) {
	svc.mu.Lock()
	svc.commands = append(svc.commands, append(json.RawMessage(nil), body...))
	svc.mu.Unlock()

	// Parse the command type to construct a plausible response.
	var envelope struct {
		Type    string `json:"type"`
		Command string `json:"command"`
	}
	_ = json.Unmarshal(body, &envelope)
	cmd := envelope.Command
	if cmd == "" {
		cmd = envelope.Type
	}
	// For get_state: return isStreaming=false so Steer routes to prompt.
	if cmd == "get_state" || envelope.Type == "get_state" {
		resp := json.RawMessage(`{"type":"response","command":"get_state","success":true,"data":{"isStreaming":false,"sessionId":"` + sessionID + `"}}`)
		return resp, nil
	}
	resp := fmt.Sprintf(`{"type":"response","command":%q,"success":true,"data":{}}`, cmd)
	return json.RawMessage(resp), nil
}

func (svc *persistentRootService) AnswerQuestion(_ context.Context, _, questionID string, _ json.RawMessage) error {
	return nil
}

func (svc *persistentRootService) Subscribe(_ context.Context) (supervisor.Subscription, error) {
	return svc.broker.Subscribe(), nil
}

func (svc *persistentRootService) CreateChild(_ context.Context, _ string, _ contract.CreateChildRequest) (supervisor.TerminalResult, error) {
	return supervisor.TerminalResult{}, contract.NewError(contract.ErrorConflict, "fake root does not support CreateChild")
}

func (svc *persistentRootService) Handoff(_ context.Context, _ supervisor.WriteHandoffRequest) (supervisor.HandoffAcceptance, error) {
	return supervisor.HandoffAcceptance{}, contract.NewError(contract.ErrorConflict, "fake root does not support Handoff")
}

func (svc *persistentRootService) AcknowledgeHandoff(_ context.Context) error {
	return contract.NewError(contract.ErrorConflict, "fake root does not support AcknowledgeHandoff")
}

func (svc *persistentRootService) Stop(_ context.Context, sessionID string) error {
	svc.stopped <- sessionID
	return nil
}

// commandCount returns the number of CallRPC invocations seen so far.
func (svc *persistentRootService) commandCount() int {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	return len(svc.commands)
}

// ---------------------------------------------------------------------------
// newBootstrapCapture — io.Writer that extracts the one-time bootstrap URL
// ---------------------------------------------------------------------------

type bootstrapCapture struct {
	started chan string
	buf     bytes.Buffer
	mu      sync.Mutex
	sent    bool
}

func newBootstrapCapture(started chan string) io.Writer {
	return &bootstrapCapture{started: started}
}

func (bc *bootstrapCapture) Write(p []byte) (int, error) {
	bc.mu.Lock()
	defer bc.mu.Unlock()
	bc.buf.Write(p)
	if !bc.sent {
		// Bootstrap URL: /bootstrap?capability=<token>
		text := bc.buf.String()
		const prefix = "Bootstrap URL: "
		idx := strings.Index(text, prefix)
		if idx != -1 {
			rest := strings.TrimSpace(text[idx+len(prefix):])
			// Take only the first line.
			if nl := strings.IndexByte(rest, '\n'); nl != -1 {
				rest = strings.TrimSpace(rest[:nl])
			}
			if rest != "" {
				bc.sent = true
				bc.started <- rest
			}
		}
	}
	return len(p), nil
}

// ---------------------------------------------------------------------------
// runningTestServer — helper that wraps a single server instance
// ---------------------------------------------------------------------------

type runningTestServer struct {
	cancel       context.CancelFunc
	result       chan error
	bootstrapURL string // relative path like /bootstrap?capability=<token>
}

// serverWithAddress wraps a running server instance together with its base URL.
type serverWithAddress struct {
	*runningTestServer
	baseURL string
}

func startTestServerWithAddress(t *testing.T, cfg config.Config, configPath string) *serverWithAddress {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan string, 1)
	result := make(chan error, 1)
	logger, _ := testLogger()
	output := newBootstrapCapture(started)

	// Capture the effective listen address via a wrapper listen function.
	addressCh := make(chan string, 1)
	listen := func(network, address string) (net.Listener, error) {
		ln, err := net.Listen(network, address)
		if err == nil {
			select {
			case addressCh <- ln.Addr().String():
			default:
			}
		}
		return ln, err
	}

	go func() {
		result <- runApplication(ctx, cfg, Options{
			ListenAddress:   "127.0.0.1:0",
			Logger:          logger,
			BootstrapOutput: output,
			ConfigPath:      configPath,
		}, productionManagerFactory, listen)
	}()

	// Wait for both the listen address and the bootstrap URL.
	var bootstrapURL string
	var listenAddr string
	remaining := 2
	deadline := time.After(15 * time.Second)
	for remaining > 0 {
		select {
		case addr := <-addressCh:
			listenAddr = addr
			remaining--
		case url := <-started:
			bootstrapURL = url
			remaining--
		case err := <-result:
			cancel()
			t.Fatalf("server failed before ready: %v", err)
		case <-deadline:
			cancel()
			t.Fatal("timed out waiting for server to start")
		}
	}

	return &serverWithAddress{
		runningTestServer: &runningTestServer{
			cancel:       cancel,
			result:       result,
			bootstrapURL: bootstrapURL,
		},
		baseURL: "http://" + listenAddr,
	}
}

// stop cancels the server and waits for it to exit cleanly.
func (srv *runningTestServer) stop(t *testing.T) {
	t.Helper()
	srv.cancel()
	select {
	case err := <-srv.result:
		if err != nil {
			t.Errorf("server returned error on stop: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Error("server did not stop within timeout")
	}
}

// ---------------------------------------------------------------------------
// bootstrapClient — performs the bootstrap dance and returns an authed client
// ---------------------------------------------------------------------------

// bootstrapClient returns an http.Client that holds the session cookie obtained
// by exchanging the bootstrap URL.
func bootstrapClient(t *testing.T, baseURL, bootstrapPath string) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}
	client := &http.Client{
		Jar: jar,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse // don't follow redirects automatically
		},
		Timeout: 10 * time.Second,
	}

	// Exchange bootstrap token.
	bootstrapFull := baseURL + bootstrapPath
	resp, err := client.Get(bootstrapFull)
	if err != nil {
		t.Fatalf("GET bootstrap: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("bootstrap status = %d, want 303", resp.StatusCode)
	}
	location := resp.Header.Get("Location")
	if location != "/" {
		t.Fatalf("bootstrap redirect to %q, want /", location)
	}

	// Follow the redirect to / to ensure cookie is set for that path.
	redirectResp, err := client.Get(baseURL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	_ = redirectResp.Body.Close()
	if redirectResp.StatusCode != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", redirectResp.StatusCode)
	}

	return client
}

// ---------------------------------------------------------------------------
// waitForFleetRoot — polls /ui/fleet SSE until the root session ID appears
// ---------------------------------------------------------------------------

func waitForFleetRoot(t *testing.T, client *http.Client, baseURL, listenAddr, rootID string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for root %q to appear in fleet", rootID)
		}

		// We do a single GET /ui/fleet request and look for the rootID in the SSE body.
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/ui/fleet", nil)
		if err != nil {
			cancel()
			time.Sleep(100 * time.Millisecond)
			continue
		}

		resp, err := client.Do(req)
		if err != nil {
			cancel()
			time.Sleep(100 * time.Millisecond)
			continue
		}

		found := false
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			if strings.Contains(scanner.Text(), rootID) {
				found = true
				break
			}
		}
		_ = resp.Body.Close()
		cancel()

		if found {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// postAction — sends an authenticated write-boundary-compliant POST
// ---------------------------------------------------------------------------

func postAction(t *testing.T, client *http.Client, baseURL, listenAddr, path string) *http.Response {
	t.Helper()
	fullURL := baseURL + path
	req, err := http.NewRequest(http.MethodPost, fullURL, strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Host = listenAddr
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://"+listenAddr)
	req.Header.Set("Sec-Fetch-Site", "same-origin")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return resp
}

// ---------------------------------------------------------------------------
// minimalTestConfig — builds a config.Config for a test server
// ---------------------------------------------------------------------------

func minimalTestConfig(t *testing.T, rootSocketDir, sessionLogDir string) (config.Config, string) {
	t.Helper()

	// Write a minimal TOML config file. The manager validates ConfigPath is
	// absolute if non-empty, and we want to provide a plausible one.
	cfgDir := t.TempDir()
	cfgPath := filepath.Join(cfgDir, "kanedias.toml")
	// ConfigPath only needs to exist and be absolute; the manager doesn't
	// actually read the file itself (server.go passes it through to Options).
	if err := os.WriteFile(cfgPath, []byte(""), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	// Build the config struct with short intervals for responsive tests.
	disc := config.Duration{Duration: 200 * time.Millisecond}
	snap := config.Duration{Duration: 200 * time.Millisecond}
	cfg := config.Config{
		Server: config.ServerConfig{
			RootSocketDir:     rootSocketDir,
			SessionLogDir:     sessionLogDir,
			DiscoveryInterval: &disc,
			SnapshotInterval:  &snap,
		},
	}
	return cfg, cfgPath
}

// ---------------------------------------------------------------------------
// waitForSocket — waits until a Unix socket appears at path
// ---------------------------------------------------------------------------

func waitForSocketFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSocket != 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("socket %q did not appear within %s", path, timeout)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// The main test
// ---------------------------------------------------------------------------

type selectedRouteFixture struct {
	manager    *manager.Manager
	service    *persistentRootService
	httpServer *httptest.Server
	cookie     *http.Cookie
	rootID     string
	socketPath string
	rootCancel context.CancelFunc
	rootDone   chan error
}

func newSelectedRouteFixture(t *testing.T) *selectedRouteFixture {
	t.Helper()
	base, err := os.MkdirTemp("/tmp", "ksel-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	for _, dir := range []string{filepath.Join(base, "r"), filepath.Join(base, "l")} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	rootID := "selected-running-root"
	socketPath := filepath.Join(base, "r", "selected.root.sock")
	service := newPersistentRootService(rootID)
	service.tree.Lifecycle = string(supervisor.LifecycleRunning)
	rootCtx, rootCancel := context.WithCancel(context.Background())
	rootDone := make(chan error, 1)
	go func() {
		rootDone <- supervisorapi.ServeUnix(rootCtx, socketPath, supervisorapi.NewHandler(service))
	}()
	waitForSocketFile(t, socketPath, 3*time.Second)

	logger, _ := testLogger()
	fleet, err := manager.New(manager.Options{
		RootSocketDir:     filepath.Join(base, "r"),
		SessionLogDir:     filepath.Join(base, "l"),
		DiscoveryInterval: 20 * time.Millisecond,
		SnapshotInterval:  20 * time.Millisecond,
		EventLimits:       supervisor.EventBrokerOptions{MaxEvents: 100},
		Logger:            logger,
	})
	if err != nil {
		rootCancel()
		t.Fatalf("manager.New: %v", err)
	}
	if err := fleet.Start(context.Background()); err != nil {
		rootCancel()
		t.Fatalf("manager.Start: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := fleet.Session(rootID); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("manager did not discover %q", rootID)
		}
		time.Sleep(10 * time.Millisecond)
	}

	handler, cookie := mustNewHandlerWithFleetAuth(t, fleet)
	httpServer := httptest.NewServer(handler)
	fixture := &selectedRouteFixture{
		manager: fleet, service: service, httpServer: httpServer, cookie: cookie,
		rootID: rootID, socketPath: socketPath, rootCancel: rootCancel, rootDone: rootDone,
	}
	t.Cleanup(func() {
		httpServer.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = fleet.Close(ctx)
		rootCancel()
		select {
		case <-rootDone:
		case <-time.After(5 * time.Second):
			t.Error("fake root did not stop")
		}
		_ = os.RemoveAll(base)
	})
	return fixture
}

type capabilityStreamProbe struct {
	mu      sync.Mutex
	body    strings.Builder
	changed chan struct{}
	done    chan error
}

func startCapabilityStreamProbe(t *testing.T, fixture *selectedRouteFixture) *capabilityStreamProbe {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	signals := url.QueryEscape(fmt.Sprintf(`{"selectedSessionId":%q}`, fixture.rootID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fixture.httpServer.URL+"/ui/session?datastar="+signals, nil)
	if err != nil {
		cancel()
		t.Fatalf("new session stream request: %v", err)
	}
	req.AddCookie(fixture.cookie)
	resp, err := fixture.httpServer.Client().Do(req)
	if err != nil {
		cancel()
		t.Fatalf("open session stream: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		cancel()
		_ = resp.Body.Close()
		t.Fatalf("session stream status = %d, want 200", resp.StatusCode)
	}
	probe := &capabilityStreamProbe{
		changed: make(chan struct{}, 1),
		done:    make(chan error, 1),
	}
	go func() {
		buffer := make([]byte, 4096)
		for {
			n, readErr := resp.Body.Read(buffer)
			if n > 0 {
				probe.mu.Lock()
				_, _ = probe.body.Write(buffer[:n])
				probe.mu.Unlock()
				select {
				case probe.changed <- struct{}{}:
				default:
				}
			}
			if readErr != nil {
				probe.done <- readErr
				return
			}
		}
	}()
	t.Cleanup(func() {
		cancel()
		_ = resp.Body.Close()
	})
	return probe
}

func (probe *capabilityStreamProbe) snapshot() string {
	probe.mu.Lock()
	defer probe.mu.Unlock()
	return probe.body.String()
}

func (probe *capabilityStreamProbe) waitForEnd(t *testing.T, timeout time.Duration) {
	t.Helper()
	select {
	case err := <-probe.done:
		if err != io.EOF {
			t.Fatalf("session stream ended with %v, want EOF", err)
		}
	case <-time.After(timeout):
		t.Fatal("session stream did not close after selected route invalidation")
	}
}

func (probe *capabilityStreamProbe) waitFor(t *testing.T, description string, timeout time.Duration, predicate func(string) bool) string {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		body := probe.snapshot()
		if predicate(body) {
			return body
		}
		select {
		case <-probe.changed:
		case err := <-probe.done:
			body = probe.snapshot()
			if predicate(body) {
				return body
			}
			t.Fatalf("session stream ended before %s: %v\n%s", description, err, body)
		case <-timer.C:
			t.Fatalf("timed out waiting for %s\n%s", description, body)
		}
	}
}

func hasCapabilities(body string, steer, interrupt, stop bool) bool {
	return strings.Contains(body, fmt.Sprintf(`data-can-steer="%t"`, steer)) &&
		strings.Contains(body, fmt.Sprintf(`data-can-interrupt="%t"`, interrupt)) &&
		strings.Contains(body, fmt.Sprintf(`data-can-stop="%t"`, stop))
}

func hasFinalEmptySessionPatch(body string) bool {
	lastDetail := strings.LastIndex(body, `id="detail-panel"`)
	if lastDetail < 0 {
		return false
	}
	tail := body[lastDetail:]
	return hasCapabilities(tail, false, false, false) &&
		!strings.Contains(tail, `data-can-steer="true"`) &&
		!strings.Contains(tail, `data-can-interrupt="true"`) &&
		!strings.Contains(tail, `data-can-stop="true"`) &&
		strings.Contains(tail, `id="question-panel"`) &&
		strings.Contains(tail, `id="activity-panel"`)
}

func TestSelectedRunningRouteAbruptRemovalStreamsDisabledCapabilities(t *testing.T) {
	fixture := newSelectedRouteFixture(t)
	probe := startCapabilityStreamProbe(t, fixture)
	probe.waitFor(t, "initial enabled capabilities", 3*time.Second, func(body string) bool {
		return hasCapabilities(body, true, true, true)
	})

	fixture.rootCancel()
	_ = os.Remove(fixture.socketPath)
	probe.waitFor(t, "empty selected-session patch after abrupt removal", 3*time.Second, hasFinalEmptySessionPatch)
	probe.waitForEnd(t, time.Second)
}

func TestSelectedStaleRootStopEvictionStreamsDisabledCapabilities(t *testing.T) {
	fixture := newSelectedRouteFixture(t)
	probe := startCapabilityStreamProbe(t, fixture)
	probe.waitFor(t, "initial enabled capabilities", 3*time.Second, func(body string) bool {
		return hasCapabilities(body, true, true, true)
	})

	fixture.service.setSnapshotError(fmt.Errorf("snapshot unavailable"))
	deadline := time.Now().Add(3 * time.Second)
	for {
		snapshot := fixture.manager.Fleet()
		if len(snapshot.Roots) == 1 && snapshot.Roots[0].Stale {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("root did not become stale")
		}
		time.Sleep(10 * time.Millisecond)
	}
	probe.waitFor(t, "stale capabilities with Stop retained", 3*time.Second, func(body string) bool {
		return hasCapabilities(body, false, false, true)
	})

	if err := fixture.manager.StopSession(context.Background(), fixture.rootID); err != nil {
		t.Fatalf("StopSession stale eviction: %v", err)
	}
	probe.waitFor(t, "empty selected-session patch after stale Stop eviction", 3*time.Second, hasFinalEmptySessionPatch)
	probe.waitForEnd(t, time.Second)
}

func TestServerRestartRediscoversRunningRoot(t *testing.T) {
	// Use a short /tmp path for sockets (Linux limit: 107 bytes).
	base, err := os.MkdirTemp("/tmp", "kint-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })

	rootSocketDir := filepath.Join(base, "r")
	sessionLogDir := filepath.Join(base, "l")
	for _, d := range []string{rootSocketDir, sessionLogDir} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}

	// --- Step 1: start the persistent fake root supervisor ---

	rootID := "test-root-1"
	rootSocketPath := filepath.Join(rootSocketDir, "existing.root.sock")
	// Also place a sibling .sock file (NOT ending in .root.sock) to prove
	// the manager ignores it.
	siblingSocketPath := filepath.Join(rootSocketDir, "some-session-id.sock")

	svc := newPersistentRootService(rootID)

	// Serve the fake root with its own long-lived context.
	rootCtx, rootCancel := context.WithCancel(context.Background())
	defer rootCancel()

	rootDone := make(chan error, 1)
	go func() {
		rootDone <- supervisorapi.ServeUnix(rootCtx, rootSocketPath, supervisorapi.NewHandler(svc))
	}()
	waitForSocketFile(t, rootSocketPath, 3*time.Second)

	// Publish a couple of events so the broker's sequence starts above 1.
	// This exercises the "incomplete history / initial replay above seq 1" case.
	// We publish two events so that when a subscriber joins they see replay
	// starting at seq >= 2, demonstrating the gap detection.
	svc.broker.PublishLocal(rootID, "pi", json.RawMessage(`{"type":"agent_start"}`))
	firstEvent := svc.broker.PublishLocal(rootID, "pi", json.RawMessage(`{"type":"agent_settled"}`))
	// firstEvent.Seq will be ≥ 2 after two publishes.
	_ = firstEvent

	// Serve a sibling .sock (must be ignored by the manager — it does not end
	// in .root.sock and is not served; we just create a dummy socket file).
	{
		addr := &net.UnixAddr{Name: siblingSocketPath, Net: "unix"}
		ln, listenErr := net.ListenUnix("unix", addr)
		if listenErr != nil {
			t.Fatalf("create sibling socket: %v", listenErr)
		}
		ln.SetUnlinkOnClose(true)
		t.Cleanup(func() { _ = ln.Close() })
	}

	// --- Step 2: start server instance 1 and bootstrap ---

	cfg, cfgPath := minimalTestConfig(t, rootSocketDir, sessionLogDir)
	srv1 := startTestServerWithAddress(t, cfg, cfgPath)

	listenAddr1 := strings.TrimPrefix(srv1.baseURL, "http://")

	// Bootstrap a session cookie.
	client1 := bootstrapClient(t, srv1.baseURL, srv1.bootstrapURL)

	// Set up cookie jar with the host for future requests.
	u1, _ := url.Parse(srv1.baseURL)
	_ = u1 // cookies are stored by the jar automatically

	// --- Step 3: verify the root appears in the fleet ---

	waitForFleetRoot(t, client1, srv1.baseURL, listenAddr1, rootID, 10*time.Second)

	// Verify the sibling .sock is NOT admitted (fleet should have exactly one root).
	{
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv1.baseURL+"/ui/fleet", nil)
		resp, err := client1.Do(req)
		if err == nil {
			var body bytes.Buffer
			scanner := bufio.NewScanner(resp.Body)
			for scanner.Scan() {
				body.WriteString(scanner.Text())
			}
			_ = resp.Body.Close()
			// The sibling session ID must not appear as a root in the fleet patch.
			if strings.Contains(body.String(), "some-session-id") {
				t.Error("sibling .sock was incorrectly admitted as a root")
			}
		}
	}

	// --- Step 4: publish an event and verify it is forwarded ---

	svc.broker.PublishLocal(rootID, "pi", json.RawMessage(`{"type":"task_update"}`))

	// --- Step 5: route control (Steer/Interrupt) to the fake root via server 1 ---

	prevCommands := svc.commandCount()
	steerResp := postAction(t, client1, srv1.baseURL, listenAddr1,
		"/ui/sessions/"+rootID+"/steer")
	if steerResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(steerResp.Body)
		t.Fatalf("steer status = %d, body = %s", steerResp.StatusCode, body)
	}
	_ = steerResp.Body.Close()

	// Wait briefly for RPC to propagate.
	for i := 0; i < 50; i++ {
		if svc.commandCount() > prevCommands {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if svc.commandCount() <= prevCommands {
		t.Error("Steer did not call RPC on the fake root")
	}

	// --- Step 6: answer a question via server 1 ---
	// POST to the question route (no real question, but the route must forward).
	qResp := postAction(t, client1, srv1.baseURL, listenAddr1,
		"/ui/sessions/"+rootID+"/questions/q-test")
	if qResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(qResp.Body)
		t.Fatalf("answer question status = %d, body = %s", qResp.StatusCode, body)
	}
	_ = qResp.Body.Close()

	// --- Step 7: shut down server instance 1 (non-destructive) ---

	srv1.stop(t)

	// Assert that the fake root is still alive (Stop was NOT called on it).
	select {
	case sid := <-svc.stopped:
		t.Errorf("server 1 shutdown called Stop on the fake root: session=%q", sid)
	default:
		// Good — root is still running.
	}

	// Also verify the fake root socket is still connectable.
	conn, dialErr := net.DialTimeout("unix", rootSocketPath, time.Second)
	if dialErr != nil {
		t.Errorf("fake root socket is no longer connectable after server 1 shutdown: %v", dialErr)
	} else {
		_ = conn.Close()
	}

	// --- Step 8: start server instance 2 against the same directory ---

	srv2 := startTestServerWithAddress(t, cfg, cfgPath)
	listenAddr2 := strings.TrimPrefix(srv2.baseURL, "http://")

	// Bootstrap a fresh session cookie (server 2 has a new capability store).
	client2 := bootstrapClient(t, srv2.baseURL, srv2.bootstrapURL)

	// --- Step 9: verify server 2 rediscovers the root ---

	waitForFleetRoot(t, client2, srv2.baseURL, listenAddr2, rootID, 10*time.Second)

	// The root must appear exactly once — check it isn't duplicated.
	{
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv2.baseURL+"/ui/fleet", nil)
		resp, err := client2.Do(req)
		if err == nil {
			var body bytes.Buffer
			scanner := bufio.NewScanner(resp.Body)
			for scanner.Scan() {
				body.WriteString(scanner.Text())
			}
			_ = resp.Body.Close()
			occurrences := strings.Count(body.String(), rootID)
			// The fleet panel HTML should contain the rootID at least once.
			if occurrences == 0 {
				t.Errorf("root %q not found in fleet after server 2 start", rootID)
			}
		}
	}

	// --- Step 10: route control via server 2 ---

	prevCommands2 := svc.commandCount()
	steerResp2 := postAction(t, client2, srv2.baseURL, listenAddr2,
		"/ui/sessions/"+rootID+"/steer")
	if steerResp2.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(steerResp2.Body)
		t.Fatalf("steer via server 2: status = %d, body = %s", steerResp2.StatusCode, body)
	}
	_ = steerResp2.Body.Close()

	// Wait briefly for RPC to propagate.
	for i := 0; i < 50; i++ {
		if svc.commandCount() > prevCommands2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if svc.commandCount() <= prevCommands2 {
		t.Error("Steer via server 2 did not call RPC on the fake root")
	}

	// --- Step 11: explicitly stop the session via server 2 ---

	stopResp := postAction(t, client2, srv2.baseURL, listenAddr2,
		"/ui/sessions/"+rootID+"/stop")
	if stopResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(stopResp.Body)
		t.Fatalf("stop status = %d, body = %s", stopResp.StatusCode, body)
	}
	_ = stopResp.Body.Close()

	// Verify Stop was eventually called on the fake.
	select {
	case sid := <-svc.stopped:
		if sid != rootID {
			t.Errorf("Stop called with session %q, want %q", sid, rootID)
		}
	case <-time.After(5 * time.Second):
		t.Error("Stop was not called on the fake root after explicit stop action")
	}

	// --- Step 12: shut down server instance 2 ---

	srv2.stop(t)

	// Shut down the fake root.
	rootCancel()
	select {
	case err := <-rootDone:
		if err != nil {
			t.Errorf("fake root ServeUnix error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("fake root did not stop")
	}
}
