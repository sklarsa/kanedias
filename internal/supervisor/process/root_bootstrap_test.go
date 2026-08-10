package process

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/sklarsa/kanedias/internal/config"
)

func validRootPolicy() config.SessionModelPolicy {
	return config.SessionModelPolicy{
		Root: config.ModelProfile{Provider: "openai-codex", Model: "gpt-5.6-sol", ThinkingLevel: "high"},
		Workers: map[string]config.WorkerProfile{
			"worker": {Description: "implementation", Provider: "openai-codex", Model: "gpt-5.6-sol", ThinkingLevel: "medium"},
		},
	}
}

func TestRootBootstrapStrictBoundedPolicy(t *testing.T) {
	bootstrap := RootBootstrap{
		Policy:    validRootPolicy(),
		Workspace: config.WorkspaceStart{Repository: "owner/repo", Checkout: "repo"},
	}
	var wire bytes.Buffer
	if err := EncodeRootBootstrap(&wire, bootstrap); err != nil {
		t.Fatal(err)
	}
	got, err := DecodeRootBootstrap(&wire)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Policy, bootstrap.Policy) || got.Workspace != bootstrap.Workspace {
		t.Fatalf("got %#v", got)
	}

	raw, err := json.Marshal(bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw[:len(raw)-1], []byte(`,"unknown":true}`)...)
	if _, err := DecodeRootBootstrap(bytes.NewReader(raw)); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown field error = %v", err)
	}
	if _, err := DecodeRootBootstrap(strings.NewReader(string(mustJSON(t, bootstrap)) + ` {}`)); err == nil || !strings.Contains(err.Error(), "multiple JSON values") {
		t.Fatalf("trailing value error = %v", err)
	}
	if _, err := DecodeRootBootstrap(strings.NewReader(strings.Repeat(" ", MaxRecordBytes+1))); !errors.Is(err, ErrRecordTooLarge) {
		t.Fatalf("oversize error = %v, want %v", err, ErrRecordTooLarge)
	}
}

func TestRootBootstrapWorkspaceValidationAndIndependence(t *testing.T) {
	zero := RootBootstrap{Policy: validRootPolicy()}
	var zeroWire bytes.Buffer
	if err := EncodeRootBootstrap(&zeroWire, zero); err != nil {
		t.Fatalf("zero workspace encode: %v", err)
	}
	gotZero, err := DecodeRootBootstrap(&zeroWire)
	if err != nil {
		t.Fatalf("zero workspace decode: %v", err)
	}
	if gotZero.Workspace != (config.WorkspaceStart{}) {
		t.Fatalf("zero workspace = %#v", gotZero.Workspace)
	}

	for _, workspace := range []config.WorkspaceStart{
		{Repository: "owner/repo", Checkout: "other"},
		{Repository: "owner/repo", Checkout: "../repo"},
	} {
		bootstrap := RootBootstrap{Policy: validRootPolicy(), Workspace: workspace}
		if err := EncodeRootBootstrap(&bytes.Buffer{}, bootstrap); err == nil {
			t.Fatalf("EncodeRootBootstrap accepted invalid workspace %#v", workspace)
		}
		if _, err := DecodeRootBootstrap(bytes.NewReader(mustJSON(t, bootstrap))); err == nil {
			t.Fatalf("DecodeRootBootstrap accepted invalid workspace %#v", workspace)
		}
	}

	bootstrap := RootBootstrap{
		Policy:    validRootPolicy(),
		Workspace: config.WorkspaceStart{Repository: "owner/repo", Checkout: "repo"},
	}
	var wire bytes.Buffer
	if err := EncodeRootBootstrap(&wire, bootstrap); err != nil {
		t.Fatal(err)
	}
	bootstrap.Workspace = config.WorkspaceStart{Repository: "other/project", Checkout: "project"}
	decoded, err := DecodeRootBootstrap(&wire)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Workspace != (config.WorkspaceStart{Repository: "owner/repo", Checkout: "repo"}) {
		t.Fatalf("decoded workspace changed with caller: %#v", decoded.Workspace)
	}
	decoded.Workspace.Checkout = "mutated"
	if bootstrap.Workspace.Checkout == decoded.Workspace.Checkout {
		t.Fatal("decoded workspace aliases caller value")
	}
}

func TestRootBootstrapRejectsStructurallyInvalidPolicies(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*config.SessionModelPolicy)
		wantErr string
	}{
		{name: "missing root provider", mutate: func(policy *config.SessionModelPolicy) { policy.Root.Provider = "" }, wantErr: "provider"},
		{name: "missing workers", mutate: func(policy *config.SessionModelPolicy) { policy.Workers = nil }, wantErr: "worker"},
		{name: "invalid worker thinking", mutate: func(policy *config.SessionModelPolicy) {
			worker := policy.Workers["worker"]
			worker.ThinkingLevel = "extreme"
			policy.Workers["worker"] = worker
		}, wantErr: "thinking level"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			invalid := validRootPolicy()
			test.mutate(&invalid)
			if err := EncodeRootBootstrap(&bytes.Buffer{}, RootBootstrap{Policy: invalid}); err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("EncodeRootBootstrap invalid policy error = %v", err)
			}

			raw := mustJSON(t, RootBootstrap{Policy: invalid})
			if _, err := DecodeRootBootstrap(bytes.NewReader(raw)); err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("DecodeRootBootstrap invalid policy error = %v", err)
			}
		})
	}
}

func TestRootBootstrapEncodeRejectsOversizePolicy(t *testing.T) {
	policy := validRootPolicy()
	worker := policy.Workers["worker"]
	worker.Description = strings.Repeat("x", MaxRecordBytes)
	policy.Workers["worker"] = worker
	if err := EncodeRootBootstrap(&bytes.Buffer{}, RootBootstrap{Policy: policy}); !errors.Is(err, ErrRecordTooLarge) {
		t.Fatalf("oversize encode error = %v, want %v", err, ErrRecordTooLarge)
	}
}

type shortRootBootstrapWriter struct{}

func (shortRootBootstrapWriter) Write(data []byte) (int, error) {
	return len(data) - 1, nil
}

func TestRootBootstrapEncodeRejectsShortWrite(t *testing.T) {
	if err := EncodeRootBootstrap(shortRootBootstrapWriter{}, RootBootstrap{Policy: validRootPolicy()}); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("EncodeRootBootstrap error = %v, want %v", err, io.ErrShortWrite)
	}
}

func TestRootBootstrapEncodeWritesExactlyOneValue(t *testing.T) {
	bootstrap := RootBootstrap{Policy: validRootPolicy()}
	var wire bytes.Buffer
	if err := EncodeRootBootstrap(&wire, bootstrap); err != nil {
		t.Fatal(err)
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(wire.Bytes()))
	if err := decoder.Decode(&value); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(&value); !errors.Is(err, io.EOF) {
		t.Fatalf("second decode error = %v, want EOF", err)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
