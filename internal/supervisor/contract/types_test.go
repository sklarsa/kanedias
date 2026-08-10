package contract

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestContractCanonicalFixtures(t *testing.T) {
	tests := []struct {
		name  string
		value any
	}{
		{name: "create-child-read.json", value: CreateChildRequest{
			WorkerType: "reviewer", Kind: ChildKindRead, Context: ContextFresh,
			Task: "Review the authentication design.",
		}},
		{name: "create-child-write.json", value: CreateChildRequest{
			WorkerType: "worker", Kind: ChildKindWrite, Context: ContextFork,
			Task: "Implement authentication.", Fork: &ForkSpec{
				SessionFile: "/workspace/.pi/sessions/root.jsonl",
				PiSessionID: "pi-root", LeafEntryID: "entry-42",
			},
		}},
		{name: "read-result.json", value: ReadChildResult{
			Kind: ChildKindRead, WorkerType: "reviewer", SessionID: "sess-child-read",
			Output: "The review found no blockers.",
		}},
		{name: "write-result.json", value: WriteChildResult{
			Kind: ChildKindWrite, WorkerType: "worker", SessionID: "sess-child-write",
			Repositories: []RepositoryHandoff{{
				Repository: "owner/repository", BaseCommit: "0123456789abcdef",
				Branch: "kanedias/sess-child-write/authentication", HeadCommit: "fedcba9876543210",
			}},
			Summary: "Implemented authentication changes.", Verification: []string{"go test ./..."},
		}},
		{name: "error.json", value: Error{
			Code: ErrorInvalidRequest, Message: "kind must be read or write",
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := json.MarshalIndent(tt.value, "", "  ")
			if err != nil {
				t.Fatalf("MarshalIndent() error = %v", err)
			}
			got = append(got, '\n')
			want, err := os.ReadFile(filepath.Join("testdata", tt.name))
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			if string(got) != string(want) {
				t.Fatalf("canonical JSON mismatch\ngot:\n%s\nwant:\n%s", got, want)
			}

			decoded := reflect.New(reflect.TypeOf(tt.value))
			if err := json.Unmarshal(want, decoded.Interface()); err != nil {
				t.Fatalf("Unmarshal() error = %v", err)
			}
			if !reflect.DeepEqual(decoded.Elem().Interface(), tt.value) {
				t.Fatalf("round trip = %#v, want %#v", decoded.Elem().Interface(), tt.value)
			}
		})
	}
}

func TestContractCreateChildRequestValidation(t *testing.T) {
	fork := &ForkSpec{SessionFile: "/tmp/session.jsonl", PiSessionID: "pi-1", LeafEntryID: "leaf-1"}
	validFresh := CreateChildRequest{WorkerType: "reviewer", Kind: ChildKindRead, Context: ContextFresh, Task: "Review this."}
	tests := []struct {
		name    string
		request CreateChildRequest
		wantErr string
	}{
		{name: "fresh read", request: validFresh},
		{name: "fork write", request: CreateChildRequest{WorkerType: "worker", Kind: ChildKindWrite, Context: ContextFork, Task: "Implement this.", Fork: fork}},
		{name: "root kind forbidden", request: CreateChildRequest{WorkerType: "worker", Kind: ChildKindRoot, Context: ContextFresh, Task: "Work."}, wantErr: "kind must be read or write"},
		{name: "unknown kind", request: CreateChildRequest{WorkerType: "worker", Kind: "audit", Context: ContextFresh, Task: "Work."}, wantErr: "kind must be read or write"},
		{name: "root context forbidden", request: CreateChildRequest{WorkerType: "reviewer", Kind: ChildKindRead, Context: ContextRoot, Task: "Review."}, wantErr: "context must be fresh or fork"},
		{name: "unknown context", request: CreateChildRequest{WorkerType: "reviewer", Kind: ChildKindRead, Context: "copy", Task: "Review."}, wantErr: "context must be fresh or fork"},
		{name: "fork requires details", request: CreateChildRequest{WorkerType: "reviewer", Kind: ChildKindRead, Context: ContextFork, Task: "Review."}, wantErr: "fork is required"},
		{name: "fresh forbids fork", request: CreateChildRequest{WorkerType: "reviewer", Kind: ChildKindRead, Context: ContextFresh, Task: "Review.", Fork: fork}, wantErr: "fork is forbidden"},
		{name: "fork fields required", request: CreateChildRequest{WorkerType: "reviewer", Kind: ChildKindRead, Context: ContextFork, Task: "Review.", Fork: &ForkSpec{}}, wantErr: "fork.sessionFile is required"},
		{name: "worker required", request: CreateChildRequest{Kind: ChildKindRead, Context: ContextFresh, Task: "Review."}, wantErr: "workerType is required"},
		{name: "task required", request: CreateChildRequest{WorkerType: "reviewer", Kind: ChildKindRead, Context: ContextFresh}, wantErr: "task is required"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.request.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestContractResultKindValidation(t *testing.T) {
	if err := (ReadChildResult{Kind: ChildKindRead}).Validate(); err != nil {
		t.Fatalf("read Validate() error = %v", err)
	}
	if err := (ReadChildResult{Kind: ChildKindWrite}).Validate(); err == nil {
		t.Fatal("write kind accepted for ReadChildResult")
	}
	if err := (WriteChildResult{Kind: ChildKindWrite}).Validate(); err != nil {
		t.Fatalf("write Validate() error = %v", err)
	}
	if err := (WriteChildResult{Kind: ChildKindRead}).Validate(); err == nil {
		t.Fatal("read kind accepted for WriteChildResult")
	}
}

func TestContractErrorHTTPStatusMappings(t *testing.T) {
	tests := []struct {
		code ErrorCode
		want int
	}{
		{ErrorInvalidRequest, http.StatusBadRequest},
		{ErrorUnknownWorkerType, http.StatusBadRequest},
		{ErrorForbiddenRPC, http.StatusConflict},
		{ErrorProxyUnavailable, http.StatusBadGateway},
		{ErrorWorkspaceRepositoryUnavailable, http.StatusServiceUnavailable},
		{ErrorProvisioningFailed, http.StatusInternalServerError},
		{ErrorChildFailed, http.StatusBadGateway},
		{ErrorChildAborted, http.StatusConflict},
		{ErrorHandoffRefMissing, http.StatusConflict},
		{ErrorHandoffRefMismatch, http.StatusConflict},
		{ErrorSessionStopping, http.StatusConflict},
		{ErrorNotFound, http.StatusNotFound},
		{ErrorChildUnavailable, http.StatusBadGateway},
		{ErrorConflict, http.StatusConflict},
		{ErrorSaturated, http.StatusTooManyRequests},
		{ErrorInternal, http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(string(tt.code), func(t *testing.T) {
			if got := tt.code.HTTPStatus(); got != tt.want {
				t.Fatalf("HTTPStatus() = %d, want %d", got, tt.want)
			}
		})
	}
	if got := ErrorCode("future_code").HTTPStatus(); got != http.StatusInternalServerError {
		t.Fatalf("unknown HTTPStatus() = %d, want %d", got, http.StatusInternalServerError)
	}
}

func TestContractTypedError(t *testing.T) {
	err := &Error{Code: ErrorConflict, Message: "already complete"}
	if got, want := err.Error(), "conflict: already complete"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
	if got := HTTPStatus(err); got != http.StatusConflict {
		t.Fatalf("HTTPStatus(error) = %d, want %d", got, http.StatusConflict)
	}
	if got := HTTPStatus(os.ErrNotExist); got != http.StatusInternalServerError {
		t.Fatalf("HTTPStatus(untyped) = %d, want %d", got, http.StatusInternalServerError)
	}
}
