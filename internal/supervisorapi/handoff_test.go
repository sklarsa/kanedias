package supervisorapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sklarsa/kanedias/internal/supervisor"
	"github.com/sklarsa/kanedias/internal/supervisor/contract"
)

func TestHandoffEndpointUsesOwnSocketIdentityAndStrictDurableBody(t *testing.T) {
	service := &fakeService{handoffResult: supervisor.HandoffAcceptance{Accepted: true, SessionID: "writer-own"}}
	handler := NewHandler(service)
	body := `{"repositories":[{"repository":"owner/one","baseCommit":"base1","branch":"feature/one","headCommit":"head1"},{"repository":"owner/two","baseCommit":"base2","branch":"feature/two","headCommit":"head2"}],"summary":"done","verification":["npm test"]}`
	response := jsonRequest(t, handler, http.MethodPost, "/v1/handoff", body)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	var accepted supervisor.HandoffAcceptance
	if err := json.Unmarshal(response.Body.Bytes(), &accepted); err != nil {
		t.Fatal(err)
	}
	if !accepted.Accepted || accepted.SessionID != "writer-own" {
		t.Fatalf("accepted = %#v", accepted)
	}
	if len(service.handoffRequest.Repositories) != 2 {
		t.Fatalf("handoff = %#v", service.handoffRequest)
	}

	for _, invalid := range []string{
		`{"sessionId":"attacker","repositories":[{"repository":"owner/repo","baseCommit":"base","branch":"feature","headCommit":"head"}],"summary":"done","verification":[]}`,
		`{"repositories":[{"path":"/workspace/repos/owner/repo","repository":"owner/repo","baseCommit":"base","branch":"feature","headCommit":"head"}],"summary":"done","verification":[]}`,
	} {
		response = jsonRequest(t, handler, http.MethodPost, "/v1/handoff", invalid)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("invalid body status = %d body=%s", response.Code, response.Body.String())
		}
	}
}

func (service *fakeService) Handoff(_ context.Context, request supervisor.WriteHandoffRequest) (supervisor.HandoffAcceptance, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.handoffRequest = request
	return service.handoffResult, service.err
}

func (service *fakeService) AcknowledgeHandoff(context.Context) error {
	if service.ackCalled != nil {
		close(service.ackCalled)
	}
	return nil
}

type blockingHandoffWriter struct {
	header  http.Header
	entered chan struct{}
	release chan struct{}
	fail    bool
}

func (writer *blockingHandoffWriter) Header() http.Header { return writer.header }
func (*blockingHandoffWriter) WriteHeader(int)            {}
func (writer *blockingHandoffWriter) Write(body []byte) (int, error) {
	close(writer.entered)
	<-writer.release
	if writer.fail {
		return 0, errors.New("blocked writer failed")
	}
	return len(body), nil
}
func (*blockingHandoffWriter) Flush() {}

func TestHandoffAcknowledgementCallbackWaitsForResponseWrite(t *testing.T) {
	service := &fakeService{
		handoffResult: supervisor.HandoffAcceptance{Accepted: true, SessionID: "writer"},
		ackCalled:     make(chan struct{}),
	}
	handler := NewHandler(service)
	request := httptest.NewRequest(http.MethodPost, "/v1/handoff", strings.NewReader(`{"repositories":[{"repository":"owner/repo","baseCommit":"base","branch":"feature","headCommit":"head"}],"summary":"done","verification":[]}`))
	request.Header.Set("Content-Type", "application/json")
	writer := &blockingHandoffWriter{header: make(http.Header), entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan struct{})
	go func() { handler.ServeHTTP(writer, request); close(done) }()
	select {
	case <-writer.entered:
	case <-time.After(time.Second):
		t.Fatal("handoff response write did not block")
	}
	select {
	case <-service.ackCalled:
		t.Fatal("handoff watchdog acknowledgement ran before response write completed")
	default:
	}
	close(writer.release)
	select {
	case <-service.ackCalled:
	case <-time.After(time.Second):
		t.Fatal("handoff acknowledgement callback was not invoked")
	}
	<-done

	failedService := &fakeService{handoffResult: supervisor.HandoffAcceptance{Accepted: true, SessionID: "writer"}, ackCalled: make(chan struct{})}
	failedWriter := &blockingHandoffWriter{header: make(http.Header), entered: make(chan struct{}), release: make(chan struct{}), fail: true}
	failedDone := make(chan struct{})
	failedRequest := httptest.NewRequest(http.MethodPost, "/v1/handoff", strings.NewReader(`{"repositories":[{"repository":"owner/repo","baseCommit":"base","branch":"feature","headCommit":"head"}],"summary":"done","verification":[]}`))
	failedRequest.Header.Set("Content-Type", "application/json")
	go func() {
		handler := NewHandler(failedService)
		handler.ServeHTTP(failedWriter, failedRequest)
		close(failedDone)
	}()
	<-failedWriter.entered
	close(failedWriter.release)
	<-failedDone
	select {
	case <-failedService.ackCalled:
		t.Fatal("failed response write acknowledged handoff")
	default:
	}
}

var _ = contract.ChildKindWrite
