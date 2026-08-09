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
	bootstrap := RootBootstrap{Policy: validRootPolicy()}
	var wire bytes.Buffer
	if err := EncodeRootBootstrap(&wire, bootstrap); err != nil {
		t.Fatal(err)
	}
	got, err := DecodeRootBootstrap(&wire)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Policy, bootstrap.Policy) {
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
