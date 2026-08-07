package supervisorapi

import (
	"context"
	"encoding/json"
	"errors"
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

	"github.com/sklarsa/kanedias/internal/supervisor"
	"github.com/sklarsa/kanedias/internal/supervisor/contract"
)

type fakeService struct {
	mu             sync.Mutex
	snapshot       supervisor.NodeSnapshot
	workers        []contract.WorkerSummary
	rpcSession     string
	rpcBody        json.RawMessage
	answerSession  string
	answerID       string
	answerBody     json.RawMessage
	sub            supervisor.Subscription
	stopSession    string
	stopCalled     chan struct{}
	stopMayObserve <-chan struct{}
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
	return json.RawMessage(`{"type":"response","success":true}`), service.err
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

func TestHandlerRejectsWrongMethodsContentTypeOversizeAndHTML(t *testing.T) {
	handler := NewHandler(&fakeService{})
	for _, test := range []struct {
		name, method, path, contentType, body string
		want                                  int
	}{
		{"method", http.MethodPost, "/v1/tree", "application/json", `{}`, http.StatusMethodNotAllowed},
		{"content-type", http.MethodPost, "/v1/sessions/self/rpc", "text/plain", `{}`, http.StatusUnsupportedMediaType},
		{"oversize", http.MethodPost, "/v1/sessions/self/rpc", "application/json", strings.Repeat("x", MaxRequestBodyBytes+1), http.StatusRequestEntityTooLarge},
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
