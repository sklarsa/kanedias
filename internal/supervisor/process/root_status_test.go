package process

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/sklarsa/kanedias/internal/supervisor/contract"
)

func TestRootStartupStatusStrictBoundedRoundTrip(t *testing.T) {
	for _, status := range []RootStartupStatus{
		{Status: RootStartupReady},
		{Status: RootStartupFailure, Code: contract.ErrorWorkspaceRepositoryUnavailable},
		{Status: RootStartupFailure, Code: contract.ErrorInternal},
	} {
		var wire bytes.Buffer
		if err := EncodeRootStartupStatus(&wire, status); err != nil {
			t.Fatalf("EncodeRootStartupStatus(%#v): %v", status, err)
		}
		got, err := DecodeRootStartupStatus(&wire)
		if err != nil {
			t.Fatalf("DecodeRootStartupStatus(%#v): %v", status, err)
		}
		if !reflect.DeepEqual(got, status) {
			t.Fatalf("round trip = %#v, want %#v", got, status)
		}
		if strings.Contains(wire.String(), "message") {
			t.Fatalf("wire record contains a free-form message: %q", wire.String())
		}
	}
}

func TestRootStartupStatusRejectsInvalidRecords(t *testing.T) {
	tests := []struct {
		name string
		wire string
	}{
		{name: "unknown status", wire: `{"status":"starting"}`},
		{name: "ready with code", wire: `{"status":"ready","code":"internal"}`},
		{name: "failure missing code", wire: `{"status":"failure"}`},
		{name: "failure unknown code", wire: `{"status":"failure","code":"future_code"}`},
		{name: "unknown field", wire: `{"status":"ready","message":"secret"}`},
		{name: "trailing value", wire: `{"status":"ready"} {}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := DecodeRootStartupStatus(strings.NewReader(test.wire)); err == nil {
				t.Fatalf("DecodeRootStartupStatus(%q) succeeded", test.wire)
			}
		})
	}
}

func TestRootStartupStatusRejectsInvalidEncode(t *testing.T) {
	for _, status := range []RootStartupStatus{
		{Status: "starting"},
		{Status: RootStartupReady, Code: contract.ErrorInternal},
		{Status: RootStartupFailure},
		{Status: RootStartupFailure, Code: contract.ErrorCode("future_code")},
	} {
		if err := EncodeRootStartupStatus(io.Discard, status); err == nil {
			t.Fatalf("EncodeRootStartupStatus(%#v) succeeded", status)
		}
	}
}

func TestRootStartupStatusRejectsOversizeInput(t *testing.T) {
	if _, err := DecodeRootStartupStatus(strings.NewReader(strings.Repeat(" ", MaxRecordBytes+1))); !errors.Is(err, ErrRecordTooLarge) {
		t.Fatalf("oversize error = %v, want %v", err, ErrRecordTooLarge)
	}
}

type shortRootStatusWriter struct{}

func (shortRootStatusWriter) Write(data []byte) (int, error) { return len(data) - 1, nil }

func TestRootStartupStatusRejectsShortWrite(t *testing.T) {
	if err := EncodeRootStartupStatus(shortRootStatusWriter{}, RootStartupStatus{Status: RootStartupReady}); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short write error = %v, want %v", err, io.ErrShortWrite)
	}
}
