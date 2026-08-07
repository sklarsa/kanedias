package supervisorapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

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

var _ = contract.ChildKindWrite
