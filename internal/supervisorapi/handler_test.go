package supervisorapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sklarsa/kanedias/internal/eventmailbox"
	"github.com/sklarsa/kanedias/internal/supervisor"
	"github.com/sklarsa/kanedias/internal/supervisor/contract"
	"github.com/sklarsa/kanedias/internal/supervisor/pirpc"
)

type fakeService struct {
	mu             sync.Mutex
	snapshot       supervisor.NodeSnapshot
	workers        []contract.WorkerSummary
	rpcSession     string
	rpcBody        json.RawMessage
	rpcResponse    json.RawMessage
	answerSession  string
	answerID       string
	answerBody     json.RawMessage
	sub            supervisor.Subscription
	stopSession    string
	stopCalled     chan struct{}
	stopMayObserve <-chan struct{}
	childRequest   contract.CreateChildRequest
	childResult    supervisor.TerminalResult
	childStarted   chan struct{}
	childRelease   <-chan struct{}
	handoffRequest supervisor.WriteHandoffRequest
	handoffResult  supervisor.HandoffAcceptance
	ackCalled      chan struct{}
	err            error
}

func (service *fakeService) Snapshot(context.Context) (supervisor.NodeSnapshot, error) {
	return service.snapshot, service.err
}
func (service *fakeService) Workers(context.Context) []contract.WorkerSummary { return service.workers }
func (service *fakeService) CallRPC(_ context.Context, session string, body json.RawMessage) (json.RawMessage, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.rpcSession, service.rpcBody = session, append(json.RawMessage(nil), body...)
	response := service.rpcResponse
	if response == nil {
		response = json.RawMessage(`{"type":"response","success":true}`)
	}
	return append(json.RawMessage(nil), response...), service.err
}
func (service *fakeService) AnswerQuestion(_ context.Context, session, id string, body json.RawMessage) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.answerSession, service.answerID, service.answerBody = session, id, append(json.RawMessage(nil), body...)
	return service.err
}
func (service *fakeService) Subscribe(context.Context) (supervisor.Subscription, error) {
	return service.sub, service.err
}
func (service *fakeService) CreateChild(ctx context.Context, session string, request contract.CreateChildRequest) (supervisor.TerminalResult, error) {
	service.mu.Lock()
	service.stopSession = session
	service.childRequest = request
	service.mu.Unlock()
	if service.childStarted != nil {
		close(service.childStarted)
	}
	if service.childRelease != nil {
		select {
		case <-service.childRelease:
		case <-ctx.Done():
			return supervisor.TerminalResult{}, ctx.Err()
		}
	}
	return service.childResult, service.err
}
func (service *fakeService) Stop(_ context.Context, session string) error {
	if service.stopMayObserve != nil {
		<-service.stopMayObserve
	}
	service.mu.Lock()
	service.stopSession = session
	service.mu.Unlock()
	if service.stopCalled != nil {
		close(service.stopCalled)
	}
	return service.err
}

func jsonRequest(t *testing.T, handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func sizedRPCJSON(t *testing.T, size int) json.RawMessage {
	t.Helper()
	const prefix = `{"type":"prompt","message":"`
	const suffix = `"}`
	padding := size - len(prefix) - len(suffix)
	if padding < 0 {
		t.Fatalf("RPC JSON size %d is too small", size)
	}
	body := json.RawMessage(prefix + strings.Repeat("x", padding) + suffix)
	if len(body) != size || !json.Valid(body) {
		t.Fatalf("RPC JSON bytes = %d, valid = %t; want %d valid bytes", len(body), json.Valid(body), size)
	}
	return body
}

func TestHandlerRootRoutes(t *testing.T) {
	closed := make(chan supervisor.EventEnvelope)
	close(closed)
	service := &fakeService{
		snapshot:   supervisor.NodeSnapshot{SessionID: "self", RootSessionID: "self", Kind: contract.ChildKindRoot, Context: contract.ContextRoot, Lifecycle: "ready", Questions: []supervisor.QuestionSummary{}, Children: []supervisor.NodeSnapshot{}},
		workers:    []contract.WorkerSummary{{WorkerType: "reviewer", Description: "review"}},
		sub:        supervisor.Subscription{Replay: []supervisor.EventEnvelope{{Seq: 1, SessionID: "self", SourceSeq: 1, Kind: "pi", Payload: json.RawMessage(`{"type":"agent_start"}`)}}, Events: closed, Close: func() {}},
		stopCalled: make(chan struct{}),
	}
	handler := NewHandler(service)

	for _, route := range []struct {
		method, path, body string
		status             int
		content            string
	}{
		{http.MethodGet, "/v1/tree", "", http.StatusOK, `"sessionId":"self"`},
		{http.MethodGet, "/v1/workers", "", http.StatusOK, `"workerType":"reviewer"`},
		{http.MethodPost, "/v1/sessions/self/rpc", `{"type":"get_state"}`, http.StatusOK, `"success":true`},
		{http.MethodPost, "/v1/sessions/self/questions/q-1/response", `{"confirmed":true}`, http.StatusNoContent, ""},
		{http.MethodDelete, "/v1/sessions/self", "", http.StatusAccepted, `"status":"stopping"`},
	} {
		response := jsonRequest(t, handler, route.method, route.path, route.body)
		if response.Code != route.status || !strings.Contains(response.Body.String(), route.content) {
			t.Errorf("%s %s = %d %q, want %d containing %q", route.method, route.path, response.Code, response.Body.String(), route.status, route.content)
		}
	}
	select {
	case <-service.stopCalled:
	case <-time.After(time.Second):
		t.Fatal("DELETE did not asynchronously call Stop")
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.rpcSession != "self" || string(service.rpcBody) != `{"type":"get_state"}` {
		t.Errorf("RPC routed as (%q, %s)", service.rpcSession, service.rpcBody)
	}
	if service.answerSession != "self" || service.answerID != "q-1" || string(service.answerBody) != `{"confirmed":true}` {
		t.Errorf("answer routed as (%q, %q, %s)", service.answerSession, service.answerID, service.answerBody)
	}
}

func TestHandlerCreateChildStrictlyDecodesAndBlocksForTerminalResult(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	service := &fakeService{
		childStarted: started,
		childRelease: release,
		childResult:  supervisor.TerminalResult{Read: &contract.ReadChildResult{Kind: contract.ChildKindRead, WorkerType: "reviewer", SessionID: "child-1", Output: "done"}},
	}
	handler := NewHandler(service)
	responseDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		responseDone <- jsonRequest(t, handler, http.MethodPost, "/v1/sessions/self/children", `{"workerType":"reviewer","kind":"read","context":"fresh","task":"review"}`)
	}()
	<-started
	select {
	case <-responseDone:
		t.Fatal("POST /children returned before the terminal result")
	default:
	}
	close(release)
	response := <-responseDone
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"output":"done"`) {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}

	unknown := jsonRequest(t, handler, http.MethodPost, "/v1/sessions/self/children", `{"workerType":"reviewer","kind":"read","context":"fresh","task":"review","model":"forbidden"}`)
	if unknown.Code != http.StatusBadRequest || !strings.Contains(unknown.Body.String(), `"code":"invalid_request"`) {
		t.Fatalf("unknown field response = %d %s", unknown.Code, unknown.Body.String())
	}
}

func TestHandlerChildProvisionFailureOmitsInternalDiagnostics(t *testing.T) {
	const publicMessage = "selected workspace repository is unavailable"
	service := &fakeService{err: errors.Join(
		contract.NewError(contract.ErrorWorkspaceRepositoryUnavailable, publicMessage),
		errors.New("execute test -d /workspace/repos/repo: fatal: selected checkout is missing"),
		errors.New("delete owned child volume: permission denied"),
	)}
	response := jsonRequest(t, NewHandler(service), http.MethodPost, "/v1/sessions/self/children", `{"workerType":"reviewer","kind":"read","context":"fresh","task":"review"}`)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body=%s, want 503", response.Code, response.Body.String())
	}
	var public contract.Error
	if err := json.Unmarshal(response.Body.Bytes(), &public); err != nil {
		t.Fatal(err)
	}
	if public.Code != contract.ErrorWorkspaceRepositoryUnavailable || public.Message != publicMessage {
		t.Fatalf("public error = %#v, want exact typed generic failure", public)
	}
	for _, forbidden := range []string{"execute test", "fatal:", "delete owned child volume", "permission denied"} {
		if strings.Contains(response.Body.String(), forbidden) {
			t.Fatalf("public response %q exposed internal detail %q", response.Body.String(), forbidden)
		}
	}
}

func TestHandlerUntypedChildOwnershipFailureUsesFixedInternalCopy(t *testing.T) {
	private := []string{
		"chown kanedias:kanedias /workspace/repos",
		"/workspace/repos -> /host/private",
		"operation not permitted",
		"delete owned child volume workspace-child-1 failed",
	}
	service := &fakeService{err: errors.Join(
		fmt.Errorf("execute %s: %s: %s", private[0], private[1], private[2]),
		errors.New(private[3]),
	)}
	response := jsonRequest(t, NewHandler(service), http.MethodPost, "/v1/sessions/self/children", `{"workerType":"reviewer","kind":"read","context":"fresh","task":"review"}`)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d body=%s, want 500", response.Code, response.Body.String())
	}
	var public contract.Error
	if err := json.Unmarshal(response.Body.Bytes(), &public); err != nil {
		t.Fatal(err)
	}
	if public.Code != contract.ErrorInternal || public.Message != "internal supervisor error" {
		t.Fatalf("public error = %#v, want fixed internal copy", public)
	}
	for _, forbidden := range private {
		if strings.Contains(response.Body.String(), forbidden) {
			t.Fatalf("public response %q exposed internal detail %q", response.Body.String(), forbidden)
		}
	}
}

func TestHandlerEventsUsesStandardSSEFraming(t *testing.T) {
	closed := make(chan supervisor.EventEnvelope)
	close(closed)
	service := &fakeService{sub: supervisor.Subscription{Replay: []supervisor.EventEnvelope{{Seq: 7, SessionID: "self", SourceSeq: 3, Kind: "pi", Payload: json.RawMessage(`{"type":"agent_settled"}`)}}, Events: closed, Close: func() {}}}
	response := jsonRequest(t, NewHandler(service), http.MethodGet, "/v1/events", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	if got := response.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("content type = %q", got)
	}
	body := response.Body.String()
	if !strings.HasPrefix(body, "id: 7\n") || !strings.Contains(body, "event: pi\n") || !strings.Contains(body, "data: {\"seq\":7") || !strings.HasSuffix(body, "\n\n") {
		t.Fatalf("SSE body = %q", body)
	}
}

func TestHandlerRejectsMalformedRPCEnvelopesAsInvalidRequest(t *testing.T) {
	service := &fakeService{}
	handler := NewHandler(service)
	for _, body := range []string{`{`, `{}`, `{"type":""}`, `{"type":"   "}`, `{"type":null}`, `{"type":1}`, `null`, `[]`} {
		request := httptest.NewRequest(http.MethodPost, "/v1/sessions/self/rpc", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"invalid_request"`) {
			t.Errorf("body %q => %d %q, want typed 400", body, response.Code, response.Body.String())
		}
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.rpcBody != nil {
		t.Fatalf("invalid RPC reached service: %s", service.rpcBody)
	}
}

func TestHandlerPreservesArbitraryPiRPCFields(t *testing.T) {
	service := &fakeService{}
	body := `{"type":"prompt","message":"go","nested":{"raw":[1,true,null]}}`
	response := jsonRequest(t, NewHandler(service), http.MethodPost, "/v1/sessions/self/rpc", body)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if string(service.rpcBody) != body {
		t.Fatalf("RPC body = %s, want byte-preserved %s", service.rpcBody, body)
	}
}

func TestRPCBodyUsesImageAwareLimit(t *testing.T) {
	payload := sizedRPCJSON(t, pirpc.MaxRecordBytes)
	response := jsonRequest(t, NewHandler(&fakeService{}), http.MethodPost, "/v1/sessions/self/rpc", string(payload))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", response.Code, response.Body.String())
	}
}

func TestRPCBodyRejectsOneByteOverImageAwareLimit(t *testing.T) {
	payload := sizedRPCJSON(t, pirpc.MaxRecordBytes+1)
	response := jsonRequest(t, NewHandler(&fakeService{}), http.MethodPost, "/v1/sessions/self/rpc", string(payload))
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d; body = %s", response.Code, response.Body.String())
	}
}

func TestHandlerRejectsWrongMethodsContentTypeOversizeAndHTML(t *testing.T) {
	handler := NewHandler(&fakeService{})
	for _, test := range []struct {
		name, method, path, contentType, body string
		want                                  int
	}{
		{"method", http.MethodPost, "/v1/tree", "application/json", `{}`, http.StatusMethodNotAllowed},
		{"content-type", http.MethodPost, "/v1/sessions/self/rpc", "text/plain", `{}`, http.StatusUnsupportedMediaType},
		{"oversize", http.MethodPost, "/v1/sessions/self/questions/q-1/response", "application/json", strings.Repeat("x", MaxRequestBodyBytes+1), http.StatusRequestEntityTooLarge},
		{"html", http.MethodGet, "/", "", "", http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			if test.contentType != "" {
				request.Header.Set("Content-Type", test.contentType)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("status = %d body=%q, want %d", response.Code, response.Body.String(), test.want)
			}
			if got := response.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
				t.Fatalf("error content type = %q", got)
			}
			if strings.Contains(strings.ToLower(response.Body.String()), "<html") {
				t.Fatalf("HTML response = %q", response.Body.String())
			}
		})
	}
}

func waitForSocket(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSocket != 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("socket %q was not created", path)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestDeleteFlushesResponseOverUnixBeforeStopObservation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api.sock")
	mayObserve := make(chan struct{})
	observed := make(chan struct{})
	service := &fakeService{stopMayObserve: mayObserve, stopCalled: observed}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serverDone := make(chan error, 1)
	go func() { serverDone <- ServeUnix(ctx, path, NewHandler(service)) }()
	waitForSocket(t, path)

	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}}
	client := &http.Client{Transport: transport}
	response, err := client.Do(mustRequest(t, http.MethodDelete, "http://unix/v1/sessions/self", nil))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusAccepted || !strings.Contains(string(body), `"status":"stopping"`) {
		t.Fatalf("DELETE response = %d %q", response.StatusCode, body)
	}
	select {
	case <-observed:
		t.Fatal("Stop was observed before the client received the complete response")
	default:
	}
	close(mayObserve)
	select {
	case <-observed:
	case <-time.After(time.Second):
		t.Fatal("Stop was not invoked after response receipt")
	}
	cancel()
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func TestDescendantClientUsesUnixAPIForSnapshotEventsAndRouting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "child.sock")
	closed := make(chan supervisor.EventEnvelope)
	close(closed)
	service := &fakeService{
		snapshot:   supervisor.NodeSnapshot{SessionID: "child", RootSessionID: "root", Children: []supervisor.NodeSnapshot{}},
		sub:        supervisor.Subscription{Replay: []supervisor.EventEnvelope{{Seq: 4, SessionID: "grandchild", SourceSeq: 2, Kind: "pi", Payload: json.RawMessage(`{"type":"agent_start"}`)}}, Events: closed, Close: func() {}},
		stopCalled: make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	serverDone := make(chan error, 1)
	go func() { serverDone <- ServeUnix(ctx, path, NewHandler(service)) }()
	waitForSocket(t, path)

	seam, err := NewDescendantClient(path)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := seam.Snapshot(context.Background())
	if err != nil || snapshot.SessionID != "child" {
		t.Fatalf("Snapshot = %#v, %v", snapshot, err)
	}
	if _, err := seam.CallRPC(context.Background(), "grandchild", json.RawMessage(`{"type":"get_state"}`)); err != nil {
		t.Fatal(err)
	}
	if err := seam.AnswerQuestion(context.Background(), "grandchild", "q-1", json.RawMessage(`{"confirmed":true}`)); err != nil {
		t.Fatal(err)
	}
	subscription, err := seam.Subscribe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-subscription.Events:
		if event.SessionID != "grandchild" || event.SourceSeq != 2 {
			t.Fatalf("event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("descendant SSE event missing")
	}
	subscription.Close()
	if err := seam.Stop(context.Background(), "grandchild"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-service.stopCalled:
	case <-time.After(time.Second):
		t.Fatal("routed stop missing")
	}

	service.mu.Lock()
	if service.rpcSession != "grandchild" || service.answerSession != "grandchild" {
		t.Fatalf("rpc=%q answer=%q", service.rpcSession, service.answerSession)
	}
	service.mu.Unlock()
	cancel()
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func TestActiveParentToChildSSEBrokerCloseAllowsPromptCleanUnixShutdown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "child.sock")
	broker := supervisor.NewEventBroker()
	broker.PublishLocal("child", "pi", json.RawMessage(`{"type":"ready"}`))
	subscription := broker.Subscribe()
	service := &fakeService{sub: subscription}
	ctx, cancel := context.WithCancel(context.Background())
	serverDone := make(chan error, 1)
	go func() { serverDone <- ServeUnix(ctx, path, NewHandler(service)) }()
	waitForSocket(t, path)

	seam, err := NewDescendantClient(path)
	if err != nil {
		t.Fatal(err)
	}
	parentStream, err := seam.Subscribe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-parentStream.Events:
	case <-time.After(time.Second):
		t.Fatal("active parent-to-child SSE stream was not established")
	}

	started := time.Now()
	broker.Close()
	cancel()
	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatalf("successful child shutdown was downgraded by listener cleanup: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("active SSE forced listener shutdown toward its 5s timeout")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("active SSE shutdown took %s", elapsed)
	}
	parentStream.Close()
}

func TestDescendantClientMapsSocketFailureToGatewayError(t *testing.T) {
	seam, err := NewDescendantClient(filepath.Join(t.TempDir(), "missing.sock"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = seam.Snapshot(context.Background())
	var typed *contract.Error
	if !errors.As(err, &typed) || typed.Code != contract.ErrorChildUnavailable {
		t.Fatalf("error = %v", err)
	}
}

func mustRequest(t *testing.T, method, url string, body io.Reader) *http.Request {
	t.Helper()
	request, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func TestServeUnixModeAndUnlinkOnShutdown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api.sock")
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- ServeUnix(ctx, path, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	}()
	waitForSocket(t, path)
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode = %04o", info.Mode().Perm())
	}
	cancel()
	if err := <-result; err != nil {
		t.Fatalf("ServeUnix() error = %v", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket remains after shutdown: %v", err)
	}
}

func TestServeUnixIdentityCheckedShutdownNeverUnlinksReplacementSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api.sock")
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- ServeUnix(ctx, path, http.NotFoundHandler()) }()
	waitForSocket(t, path)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	replacement, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = replacement.Close() }()

	cancel()
	if err := <-result; err == nil || !strings.Contains(err.Error(), "refuse unlink of replaced Unix socket") {
		t.Fatalf("ServeUnix() error = %v, want replacement identity refusal", err)
	}
	connection, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("replacement socket was unlinked: %v", err)
	}
	_ = connection.Close()
}

func TestServeUnixRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	path := filepath.Join(dir, "api.sock")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	err := ServeUnix(context.Background(), path, http.NotFoundHandler())
	if err == nil {
		t.Fatal("ServeUnix accepted symlink")
	}
	if info, statErr := os.Lstat(path); statErr != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink was altered: info=%v err=%v", info, statErr)
	}
}

func TestServeUnixRemovesStaleOwnedSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api.sock")
	address, err := net.ResolveUnixAddr("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	stale, err := net.ListenUnix("unix", address)
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	if err := stale.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- ServeUnix(ctx, path, http.NotFoundHandler()) }()
	waitForSocket(t, path)
	cancel()
	if err := <-result; err != nil {
		t.Fatalf("ServeUnix() error = %v", err)
	}
}

func TestAppendDescendantSSEDataEnforcesAggregateBoundary(t *testing.T) {
	for _, test := range []struct {
		name   string
		parts  []string
		limit  int
		wantOK bool
	}{
		{name: "exact total", parts: []string{strings.Repeat("x", maxDescendantSSEEventBytes)}, limit: maxDescendantSSEEventBytes, wantOK: true},
		{name: "total plus one", parts: []string{strings.Repeat("x", maxDescendantSSEEventBytes), "y"}, limit: maxDescendantSSEEventBytes, wantOK: false},
		{name: "many small lines", parts: []string{"a", "b", "c", "d", "e", "f"}, limit: 10, wantOK: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var data strings.Builder
			var err error
			for _, part := range test.parts {
				err = appendDescendantSSEData(&data, part, test.limit)
				if err != nil {
					break
				}
			}
			if (err == nil) != test.wantOK {
				t.Fatalf("error = %v, builder bytes = %d", err, data.Len())
			}
			if data.Len() > test.limit {
				t.Fatalf("builder bytes = %d, exceeded %d", data.Len(), test.limit)
			}
		})
	}
}

func TestDescendantSSEAcceptsPiSizedEnvelopeAndSurfacesStreamErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "child.sock")
	large := json.RawMessage(`{"text":"` + strings.Repeat("x", 2<<20) + `"}`)
	closed := make(chan supervisor.EventEnvelope)
	close(closed)
	service := &fakeService{sub: supervisor.Subscription{Replay: []supervisor.EventEnvelope{{Seq: 1, SessionID: "child", SourceSeq: 1, Kind: "pi", Payload: large}}, Events: closed, Close: func() {}}}
	ctx, cancel := context.WithCancel(context.Background())
	serverDone := make(chan error, 1)
	go func() { serverDone <- ServeUnix(ctx, path, NewHandler(service)) }()
	waitForSocket(t, path)
	seam, err := NewDescendantClient(path)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := seam.Subscribe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-sub.Events:
		if len(event.Payload) != len(large) {
			t.Fatalf("payload bytes = %d, want %d", len(event.Payload), len(large))
		}
	case <-time.After(time.Second):
		t.Fatal("Pi-sized descendant event was not forwarded")
	}
	sub.Close()
	cancel()
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}

	badPath := filepath.Join(t.TempDir(), "bad.sock")
	badCtx, badCancel := context.WithCancel(context.Background())
	badDone := make(chan error, 1)
	go func() {
		badDone <- ServeUnix(badCtx, badPath, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "data: not-json\n\n")
		}))
	}()
	waitForSocket(t, badPath)
	badSeam, _ := NewDescendantClient(badPath)
	badSub, err := badSeam.Subscribe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for range badSub.Events {
	}
	if badSub.Err == nil || badSub.Err() == nil {
		t.Fatal("malformed child SSE ended without owned stream error")
	}
	badCancel()
	if err := <-badDone; err != nil {
		t.Fatal(err)
	}
}

func TestDescendantSSEPreservesOrdinaryBurst(t *testing.T) {
	path := filepath.Join(t.TempDir(), "burst.sock")
	ctx, cancel := context.WithCancel(context.Background())
	release := make(chan struct{})
	written := make(chan struct{})
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- ServeUnix(ctx, path, http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-release
			for seq := 1; seq <= 256; seq++ {
				_, _ = fmt.Fprintf(w, "data: {\"seq\":%d,\"sessionId\":\"child\",\"sourceSeq\":%d,\"kind\":\"pi\",\"payload\":{}}\n\n", seq, seq)
				w.(http.Flusher).Flush()
			}
			close(written)
			<-request.Context().Done()
		}))
	}()
	waitForSocket(t, path)
	client, err := NewClient(path)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := client.Subscribe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case <-written:
	case <-time.After(time.Second):
		t.Fatal("server did not write descendant event burst")
	}
	for want := uint64(1); want <= 256; want++ {
		select {
		case event, open := <-sub.Events:
			if !open {
				t.Fatalf("event stream closed before sequence %d: %v", want, sub.Err())
			}
			if event.Seq != want {
				t.Fatalf("event sequence = %d, want %d", event.Seq, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for sequence %d", want)
		}
	}
	if sub.Err() != nil {
		t.Fatalf("ordinary burst produced stream error: %v", sub.Err())
	}
	sub.Close()
	cancel()
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func TestDescendantSSEDisconnectsStalledConsumerAtBoundedCapacity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stalled.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	release := make(chan struct{})
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- ServeUnix(ctx, path, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-release
			for seq := 1; seq <= 3; seq++ {
				_, _ = fmt.Fprintf(w, "data: {\"seq\":%d,\"sessionId\":\"child\",\"sourceSeq\":%d,\"kind\":\"pi\",\"payload\":{}}\n\n", seq, seq)
				w.(http.Flusher).Flush()
			}
		}))
	}()
	waitForSocket(t, path)
	client, err := NewClient(path)
	if err != nil {
		t.Fatal(err)
	}
	client.eventLimits = eventmailbox.Limits{MaxEvents: 2, MaxBytes: 1024}
	sub, err := client.Subscribe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	close(release)
	deadline := time.Now().Add(time.Second)
	for sub.Err() == nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	var typed *contract.Error
	if !errors.As(sub.Err(), &typed) || typed.Code != contract.ErrorChildUnavailable || !strings.Contains(sub.Err().Error(), "event consumer") {
		t.Fatalf("stream error = %v, want child_unavailable stalled consumer failure", sub.Err())
	}
	select {
	case _, open := <-sub.Events:
		if open {
			t.Fatal("stalled descendant event channel remained open after overflow")
		}
	case <-time.After(time.Second):
		t.Fatal("stalled descendant event channel did not close promptly")
	}
	sub.Close()
	cancel()
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func TestDescendantSSECleanEOFDrainsAcceptedEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "drain.sock")
	ctx, cancel := context.WithCancel(context.Background())
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- ServeUnix(ctx, path, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			for seq := 1; seq <= 3; seq++ {
				_, _ = fmt.Fprintf(w, "data: {\"seq\":%d,\"sessionId\":\"child\",\"sourceSeq\":%d,\"kind\":\"pi\",\"payload\":{}}\n\n", seq, seq)
			}
		}))
	}()
	waitForSocket(t, path)
	client, err := NewClient(path)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := client.Subscribe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var sequences []uint64
	for event := range sub.Events {
		sequences = append(sequences, event.Seq)
	}
	if len(sequences) != 3 || sequences[0] != 1 || sequences[1] != 2 || sequences[2] != 3 {
		t.Fatalf("drained sequences = %v, want [1 2 3]", sequences)
	}
	var typed *contract.Error
	if sub.Err == nil || !errors.As(sub.Err(), &typed) || typed.Code != contract.ErrorChildUnavailable {
		t.Fatalf("clean EOF error = %v, want child_unavailable", sub.Err())
	}
	cancel()
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func TestDescendantSSECleanEOFIsOwnedFailureUnlessParentCloses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clean-eof.sock")
	ctx, cancel := context.WithCancel(context.Background())
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- ServeUnix(ctx, path, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
		}))
	}()
	waitForSocket(t, path)
	seam, err := NewDescendantClient(path)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := seam.Subscribe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for range sub.Events {
	}
	var typed *contract.Error
	if sub.Err == nil || !errors.As(sub.Err(), &typed) || typed.Code != contract.ErrorChildUnavailable {
		t.Fatalf("clean active EOF error = %v, want child_unavailable", sub.Err())
	}

	parentCtx, parentCancel := context.WithCancel(context.Background())
	parentPath := filepath.Join(t.TempDir(), "parent-close.sock")
	parentDone := make(chan error, 1)
	go func() {
		parentDone <- ServeUnix(parentCtx, parentPath, http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-request.Context().Done()
		}))
	}()
	waitForSocket(t, parentPath)
	parentSeam, _ := NewDescendantClient(parentPath)
	parentSub, err := parentSeam.Subscribe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	parentSub.Close()
	for range parentSub.Events {
	}
	if parentSub.Err != nil && parentSub.Err() != nil {
		t.Fatalf("parent close became stream failure: %v", parentSub.Err())
	}

	parentCancel()
	if err := <-parentDone; err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func TestDescendantUnaryOperationsHaveInternalDeadline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stall.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- ServeUnix(ctx, path, http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) { <-request.Context().Done() }))
	}()
	waitForSocket(t, path)
	seam, err := NewDescendantClient(path)
	if err != nil {
		t.Fatal(err)
	}
	client := seam.(*DescendantClient)
	client.unaryTimeout = 20 * time.Millisecond
	started := time.Now()
	_, err = client.Snapshot(context.Background())
	if err == nil || time.Since(started) > time.Second {
		t.Fatalf("bounded Snapshot() error=%v elapsed=%s", err, time.Since(started))
	}
	cancel()
	select {
	case <-done:
	case <-time.After(6 * time.Second):
		t.Fatal("server did not close")
	}
}

func TestDescendantClientRPCResponseUsesExactImageAwareLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rpc-response.sock")
	service := &fakeService{
		rpcResponse: sizedRPCJSON(t, pirpc.MaxRecordBytes),
		snapshot: supervisor.NodeSnapshot{
			SessionID: "self", RootSessionID: "self", SessionFile: strings.Repeat("x", MaxRequestBodyBytes), Children: []supervisor.NodeSnapshot{},
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ServeUnix(ctx, path, NewHandler(service)) }()
	waitForSocket(t, path)
	client, err := NewClient(path)
	if err != nil {
		t.Fatal(err)
	}

	response, err := client.CallRPC(context.Background(), "self", json.RawMessage(`{"type":"get_state"}`))
	if err != nil {
		t.Fatalf("exact-limit CallRPC() error = %v", err)
	}
	if len(response) != pirpc.MaxRecordBytes {
		t.Fatalf("CallRPC() response bytes = %d, want %d", len(response), pirpc.MaxRecordBytes)
	}

	oversizedResponse := sizedRPCJSON(t, pirpc.MaxRecordBytes+1)
	service.mu.Lock()
	service.rpcResponse = oversizedResponse
	service.mu.Unlock()
	if _, err := client.CallRPC(context.Background(), "self", json.RawMessage(`{"type":"get_state"}`)); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("one-byte-over CallRPC() error = %v, want size rejection", err)
	}
	if _, err := client.Snapshot(context.Background()); err == nil || !strings.Contains(err.Error(), "exceeds 1 MiB") {
		t.Fatalf("Snapshot() error = %v, want 1 MiB response rejection", err)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestDescendantClientResponseBoundaryRejectsTrailingData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "boundary.sock")
	snapshot := supervisor.NodeSnapshot{SessionID: "self", RootSessionID: "self", Children: []supervisor.NodeSnapshot{}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- ServeUnix(ctx, path, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			enc := json.NewEncoder(w)
			_ = enc.Encode(snapshot)
			// Write a second JSON value — whole-body unmarshal must reject this.
			_ = enc.Encode(map[string]string{"extra": "field"})
		}))
	}()
	waitForSocket(t, path)
	client, err := NewClient(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Snapshot(context.Background())
	if err == nil {
		t.Fatal("client accepted response with trailing JSON data")
	}
	var typed *contract.Error
	if !errors.As(err, &typed) || typed.Code != contract.ErrorChildUnavailable {
		t.Fatalf("error = %v, want child_unavailable", err)
	}
}
