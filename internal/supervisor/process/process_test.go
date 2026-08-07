//go:build unix

package process

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sklarsa/kanedias/internal/config"
	"github.com/sklarsa/kanedias/internal/supervisor/contract"
)

func validBootstrap(t *testing.T) Bootstrap {
	t.Helper()
	return Bootstrap{
		SessionID: "child-1", ParentID: "parent-1", RootID: "root-1",
		SocketPath:     filepath.Join(t.TempDir(), "child.sock"),
		SourceInstance: "session-parent-1", SourceVolume: "workspace-parent-1",
		Worker:  config.WorkerProfile{Description: "Review code", Provider: "openai-codex", Model: "gpt-5", ThinkingLevel: "high"},
		Request: contract.CreateChildRequest{WorkerType: "reviewer", Kind: contract.ChildKindRead, Context: contract.ContextFresh, Task: "review this change"},
	}
}

func TestBootstrapStrictDecodeAndValidation(t *testing.T) {
	bootstrap := validBootstrap(t)
	var encoded bytes.Buffer
	if err := EncodeBootstrap(&encoded, bootstrap); err != nil {
		t.Fatal(err)
	}
	got, err := DecodeBootstrap(&encoded)
	if err != nil {
		t.Fatal(err)
	}
	if got.SessionID != bootstrap.SessionID || got.Request.Task != bootstrap.Request.Task || got.Worker.Model != bootstrap.Worker.Model {
		t.Fatalf("decoded bootstrap = %#v", got)
	}

	data, _ := json.Marshal(bootstrap)
	data = append(data[:len(data)-1], []byte(`,"unknown":true}`)...)
	if _, err := DecodeBootstrap(bytes.NewReader(data)); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown-field error = %v", err)
	}
	if _, err := DecodeBootstrap(strings.NewReader(strings.Repeat(" ", MaxRecordBytes+1))); !errors.Is(err, ErrRecordTooLarge) {
		t.Fatalf("oversize error = %v, want ErrRecordTooLarge", err)
	}
}

func TestBootstrapRejectsInvalidInputsBeforeProvisioning(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Bootstrap)
	}{
		{"missing session identity", func(b *Bootstrap) { b.SessionID = "" }},
		{"same session and parent", func(b *Bootstrap) { b.SessionID = b.ParentID }},
		{"same session and root", func(b *Bootstrap) { b.SessionID = b.RootID }},
		{"missing source instance", func(b *Bootstrap) { b.SourceInstance = "" }},
		{"missing source volume", func(b *Bootstrap) { b.SourceVolume = "" }},
		{"relative socket", func(b *Bootstrap) { b.SocketPath = "child.sock" }},
		{"missing worker description", func(b *Bootstrap) { b.Worker.Description = "" }},
		{"missing worker provider", func(b *Bootstrap) { b.Worker.Provider = "" }},
		{"missing worker model", func(b *Bootstrap) { b.Worker.Model = "" }},
		{"invalid thinking level", func(b *Bootstrap) { b.Worker.ThinkingLevel = "huge" }},
		{"invalid fork combination", func(b *Bootstrap) { b.Request.Context = contract.ContextFork }},
		{"missing task", func(b *Bootstrap) { b.Request.Task = " " }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bootstrap := validBootstrap(t)
			tt.edit(&bootstrap)
			var data bytes.Buffer
			if err := json.NewEncoder(&data).Encode(bootstrap); err != nil {
				t.Fatal(err)
			}
			if _, err := DecodeBootstrap(&data); err == nil {
				t.Fatal("DecodeBootstrap() succeeded")
			}
		})
	}
}

func TestReporterEmitsTypedStrictJSONLMessages(t *testing.T) {
	var output bytes.Buffer
	reporter := NewReporter(&output, "child-1")
	if err := reporter.Read(contract.ReadChildResult{Kind: contract.ChildKindRead, WorkerType: "reviewer", SessionID: "child-1", Output: "done"}); err != nil {
		t.Fatal(err)
	}
	if err := reporter.Failure(contract.ErrorChildFailed, "boom"); err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(&output)
	var read, failure ChildMessage
	if err := decoder.Decode(&read); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(&failure); err != nil {
		t.Fatal(err)
	}
	if read.Type != MessageRead || read.Read == nil || read.Read.Output != "done" {
		t.Fatalf("read message = %#v", read)
	}
	if failure.Type != MessageFailure || failure.Error == nil || failure.Error.Code != contract.ErrorChildFailed {
		t.Fatalf("failure message = %#v", failure)
	}
}

func TestDecodeChildMessageRejectsUnknownFieldsAndPayloadMismatch(t *testing.T) {
	for _, record := range []string{
		`{"type":"ready","sessionId":"child-1","ready":{"socketPath":"/tmp/a"},"extra":1}`,
		`{"type":"ready","sessionId":"child-1","read":{"kind":"read","workerType":"r","sessionId":"child-1","output":"x"}}`,
		`{"type":"read","sessionId":"child-1","read":{"kind":"read","workerType":"r","sessionId":"other","output":"x"}}`,
	} {
		if _, err := DecodeChildMessage(strings.NewReader(record)); err == nil {
			t.Fatalf("DecodeChildMessage(%s) succeeded", record)
		}
	}
}

func TestSpawnerUsesOnlyInheritedProtocolDescriptorsAndProbesSocket(t *testing.T) {
	bootstrap := validBootstrap(t)
	serveTree(t, bootstrap.SocketPath)
	script := filepath.Join(t.TempDir(), "helper.sh")
	contents := fmt.Sprintf("#!/bin/sh\nset -eu\n[ \"$*\" = 'session-child --bootstrap-fd 3 --liveness-fd 4 --report-fd 5' ]\ncat <&3 >/dev/null\nprintf '%%s\\n' '%s' >&5\ncat <&4 >/dev/null\n", fmt.Sprintf(`{"type":"ready","sessionId":%q,"ready":{"socketPath":%q}}`, bootstrap.SessionID, bootstrap.SocketPath))
	if err := os.WriteFile(script, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}

	child, err := (Spawner{Executable: script, ProbeInterval: 5 * time.Millisecond}).Spawn(context.Background(), bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	defer child.Kill()
	readyCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := child.WaitReady(readyCtx); err != nil {
		t.Fatal(err)
	}
	if err := child.CloseLiveness(); err != nil {
		t.Fatal(err)
	}
	if err := child.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestSpawnerRejectsReadyUntilSessionAndSocketAreVerified(t *testing.T) {
	for _, tt := range []struct {
		name, session, socket string
	}{
		{name: "wrong session", session: "other", socket: "valid"},
		{name: "wrong socket", session: "valid", socket: "/tmp/wrong.sock"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			bootstrap := validBootstrap(t)
			serveTree(t, bootstrap.SocketPath)
			session := tt.session
			if session == "valid" {
				session = bootstrap.SessionID
			}
			socket := tt.socket
			if socket == "valid" {
				socket = bootstrap.SocketPath
			}
			script := filepath.Join(t.TempDir(), "helper.sh")
			contents := fmt.Sprintf("#!/bin/sh\ncat <&3 >/dev/null\nprintf '%%s\\n' '%s' >&5\ncat <&4 >/dev/null\n", fmt.Sprintf(`{"type":"ready","sessionId":%q,"ready":{"socketPath":%q}}`, session, socket))
			if err := os.WriteFile(script, []byte(contents), 0o700); err != nil {
				t.Fatal(err)
			}
			child, err := (Spawner{Executable: script, ProbeInterval: time.Millisecond}).Spawn(context.Background(), bootstrap)
			if err != nil {
				t.Fatal(err)
			}
			defer child.Kill()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := child.WaitReady(ctx); err == nil {
				t.Fatal("WaitReady() succeeded")
			}
		})
	}
}

func TestParentLivenessEOFCascadesThroughRealChildAndGrandchild(t *testing.T) {
	if os.Getenv("KANEDIAS_LIVENESS_HELPER") != "" {
		runLivenessHelper(t)
		return
	}
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestParentLivenessEOFCascadesThroughRealChildAndGrandchild", "--")
	cmd.Env = append(os.Environ(), "KANEDIAS_LIVENESS_HELPER=child")
	cmd.ExtraFiles = []*os.File{readEnd}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = readEnd.Close()
	if err := writeEnd.Close(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("cascade helper: %v: %s", err, stderr.String())
		}
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("child/grandchild did not exit after parent EOF; a descendant likely inherited a liveness writer: %s", stderr.String())
	}
}

func runLivenessHelper(t *testing.T) {
	t.Helper()
	liveness := os.NewFile(3, "parent-liveness")
	if liveness == nil {
		t.Fatal("missing fd 3")
	}
	if os.Getenv("KANEDIAS_LIVENESS_HELPER") == "grandchild" {
		var stopped atomic.Int32
		err := MonitorParentLiveness(context.Background(), liveness, func(context.Context) error { stopped.Add(1); return nil })
		if err != nil || stopped.Load() != 1 {
			t.Fatalf("grandchild monitor = %v, stops = %d", err, stopped.Load())
		}
		return
	}
	grandRead, grandWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestParentLivenessEOFCascadesThroughRealChildAndGrandchild", "--")
	cmd.Env = append(os.Environ(), "KANEDIAS_LIVENESS_HELPER=grandchild")
	cmd.ExtraFiles = []*os.File{grandRead}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = grandRead.Close()
	var stopped atomic.Int32
	err = MonitorParentLiveness(context.Background(), liveness, func(context.Context) error {
		stopped.Add(1)
		return grandWrite.Close()
	})
	if err != nil {
		t.Fatal(err)
	}
	if stopped.Load() != 1 {
		t.Fatalf("child stops = %d", stopped.Load())
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestMonitorParentLivenessUsesIdempotentStopPath(t *testing.T) {
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	stop := IdempotentStop(func(context.Context) error { calls.Add(1); return nil })
	if err := stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = writeEnd.Close()
	if err := MonitorParentLiveness(context.Background(), readEnd, stop); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("stop calls = %d, want 1", got)
	}
}

func serveTree(t *testing.T, socket string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/tree" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close(); _ = listener.Close() })
}
