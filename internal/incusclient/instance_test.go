package incusclient

import (
	"context"
	"errors"
	"io"
	"runtime"
	"strings"
	"testing"
	"time"

	incus "github.com/lxc/incus/v7/client"
	"github.com/lxc/incus/v7/shared/api"
)

type copyInstanceServer struct {
	incus.InstanceServer
	contextSeen  context.Context
	source       *api.Instance
	copySource   incus.InstanceServer
	copyInstance api.Instance
	copyArgs     *incus.InstanceCopyArgs
	operation    incus.RemoteOperation
	updateName   string
	updatePut    api.InstancePut
	updateETag   string
}

func (s *copyInstanceServer) WithContext(ctx context.Context) incus.InstanceServer {
	s.contextSeen = ctx
	return s
}

func (s *copyInstanceServer) GetInstance(string) (*api.Instance, string, error) {
	return s.source, "source-etag", nil
}

func (s *copyInstanceServer) CopyInstance(source incus.InstanceServer, instance api.Instance, args *incus.InstanceCopyArgs) (incus.RemoteOperation, error) {
	s.copySource = source
	s.copyInstance = instance
	s.copyArgs = args
	return s.operation, nil
}

func (s *copyInstanceServer) UpdateInstance(name string, put api.InstancePut, etag string) (incus.Operation, error) {
	s.updateName = name
	s.updatePut = put
	s.updateETag = etag
	return completedClientOperation{}, nil
}

type completedClientOperation struct {
	incus.Operation
}

func (completedClientOperation) WaitContext(context.Context) error { return nil }
func (completedClientOperation) Get() api.Operation                { return api.Operation{} }

type completedRemoteOperation struct {
	incus.RemoteOperation
}

func (completedRemoteOperation) Wait() error         { return nil }
func (completedRemoteOperation) CancelTarget() error { return nil }

func TestCopyInstanceUsesStoppedInstanceOnlyPullClone(t *testing.T) {
	ctx := context.WithValue(context.Background(), struct{}{}, "copy-context")
	server := &copyInstanceServer{
		source:    &api.Instance{Name: "parent"},
		operation: completedRemoteOperation{},
	}
	client := &Client{server: server}

	if err := client.CopyInstance(ctx, "parent", "child"); err != nil {
		t.Fatal(err)
	}
	if server.contextSeen != ctx {
		t.Fatal("CopyInstance did not scope Incus requests to the supplied context")
	}
	if server.copySource != server || server.copyInstance.Name != "parent" {
		t.Fatalf("copy source = %#v from %T, want parent from scoped server", server.copyInstance, server.copySource)
	}
	want := incus.InstanceCopyArgs{Name: "child", Mode: "pull", InstanceOnly: true, Live: false}
	if server.copyArgs == nil || *server.copyArgs != want {
		t.Fatalf("InstanceCopyArgs = %#v, want %#v", server.copyArgs, want)
	}
}

func TestUpdateInstancePreservesETag(t *testing.T) {
	server := &copyInstanceServer{}
	client := &Client{server: server}
	put := api.InstancePut{Description: "child", Config: api.ConfigMap{"key": "value"}}

	if err := client.UpdateInstance(context.Background(), "child", put, "instance-etag"); err != nil {
		t.Fatal(err)
	}
	if server.updateName != "child" || server.updateETag != "instance-etag" {
		t.Fatalf("UpdateInstance arguments = %q, %q, want child and preserved ETag", server.updateName, server.updateETag)
	}
	if server.updatePut.Description != "child" || server.updatePut.Config["key"] != "value" {
		t.Fatalf("UpdateInstance put = %#v, want supplied request", server.updatePut)
	}
}

func TestExecCapturesStdoutAndStderr(t *testing.T) {
	ctx := context.Background()
	var gotPost api.InstanceExecPost
	var gotArgs *incus.InstanceExecArgs
	call := func(post api.InstanceExecPost, args *incus.InstanceExecArgs) (operationWaiter, error) {
		gotPost = post
		gotArgs = args
		_, _ = io.WriteString(args.Stdout, "standard output")
		_, _ = io.WriteString(args.Stderr, "standard error")
		if args.DataDone != nil {
			close(args.DataDone)
		}
		return completedExecOperation(float64(0)), nil
	}

	stdout, stderr, err := exec(ctx, call, ExecRequest{
		Command:     []string{"sh", "-c", "echo"},
		Environment: map[string]string{"KEY": "value"},
		Cwd:         "/workspace",
		Stdin:       strings.NewReader("input"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "standard output" || stderr != "standard error" {
		t.Fatalf("Exec output = %q, %q", stdout, stderr)
	}
	if gotPost.Cwd != "/workspace" || !gotPost.WaitForWS || gotPost.Interactive {
		t.Fatalf("exec post = %#v", gotPost)
	}
	if gotArgs.Stdin == nil {
		t.Fatal("stdin was not passed to Incus")
	}
}

func TestExecCallFailurePreservesOutput(t *testing.T) {
	callErr := errors.New("websocket setup failed")
	call := func(_ api.InstanceExecPost, args *incus.InstanceExecArgs) (operationWaiter, error) {
		_, _ = io.WriteString(args.Stdout, "stdout before call failure")
		_, _ = io.WriteString(args.Stderr, "stderr before call failure")
		return nil, callErr
	}

	stdout, stderr, err := exec(context.Background(), call, ExecRequest{Command: []string{"echo"}})
	if !errors.Is(err, callErr) {
		t.Fatalf("Exec error = %v, want call failure", err)
	}
	if stdout != "stdout before call failure" || stderr != "stderr before call failure" {
		t.Fatalf("Exec output = %q, %q, want preserved output", stdout, stderr)
	}
}

func TestExecWaitFailureReturnsWithoutDataDone(t *testing.T) {
	waitErr := errors.New("operation wait failed")
	operation := &cancelableFakeOperation{fakeOperation: &fakeOperation{waitErr: waitErr}}
	argsSeen := make(chan *incus.InstanceExecArgs, 1)
	call := func(_ api.InstanceExecPost, args *incus.InstanceExecArgs) (operationWaiter, error) {
		argsSeen <- args
		_, _ = io.WriteString(args.Stdout, "stdout before wait failure")
		_, _ = io.WriteString(args.Stderr, "stderr before wait failure")
		return operation, nil
	}

	type result struct {
		stdout string
		stderr string
		err    error
	}
	resultCh := make(chan result, 1)
	go func() {
		stdout, stderr, err := exec(context.Background(), call, ExecRequest{Command: []string{"echo"}})
		resultCh <- result{stdout: stdout, stderr: stderr, err: err}
	}()
	args := <-argsSeen

	select {
	case got := <-resultCh:
		if !errors.Is(got.err, waitErr) {
			t.Fatalf("Exec error = %v, want wait failure", got.err)
		}
		if got.stdout != "stdout before wait failure" || got.stderr != "stderr before wait failure" {
			t.Fatalf("Exec output = %q, %q, want preserved output", got.stdout, got.stderr)
		}
		if !operation.cancelled {
			t.Fatal("Exec did not best-effort cancel the failed operation")
		}
	case <-time.After(time.Second):
		if args.DataDone != nil {
			close(args.DataDone)
		}
		<-resultCh
		t.Fatal("Exec waited for DataDone after the operation wait failed")
	}
}

func TestExecReturnsNonZeroCommandStatusWithOutput(t *testing.T) {
	call := func(_ api.InstanceExecPost, args *incus.InstanceExecArgs) (operationWaiter, error) {
		_, _ = io.WriteString(args.Stdout, "partial stdout")
		_, _ = io.WriteString(args.Stderr, "failure detail")
		if args.DataDone != nil {
			close(args.DataDone)
		}
		return completedExecOperation(float64(23)), nil
	}

	stdout, stderr, err := exec(context.Background(), call, ExecRequest{Command: []string{"false"}})
	if err == nil || !strings.Contains(err.Error(), "exit status 23") {
		t.Fatalf("Exec error = %v, want exit status 23", err)
	}
	if stdout != "partial stdout" || stderr != "failure detail" {
		t.Fatalf("Exec output = %q, %q, want preserved output", stdout, stderr)
	}
}

func TestExecWaitsForDataDoneBeforeReturningOutput(t *testing.T) {
	argsSeen := make(chan *incus.InstanceExecArgs, 1)
	allowOutput := make(chan struct{})
	call := func(_ api.InstanceExecPost, args *incus.InstanceExecArgs) (operationWaiter, error) {
		argsSeen <- args
		go func() {
			<-allowOutput
			_, _ = io.WriteString(args.Stdout, "flushed stdout")
			_, _ = io.WriteString(args.Stderr, "flushed stderr")
			if args.DataDone != nil {
				close(args.DataDone)
			}
		}()
		return completedExecOperation(float64(0)), nil
	}

	type result struct {
		stdout string
		stderr string
		err    error
	}
	resultCh := make(chan result, 1)
	go func() {
		stdout, stderr, err := exec(context.Background(), call, ExecRequest{Command: []string{"echo"}})
		resultCh <- result{stdout: stdout, stderr: stderr, err: err}
	}()

	args := <-argsSeen
	if args.DataDone == nil {
		// This is the expected RED failure against the old adapter.
		close(allowOutput)
		<-resultCh
		t.Fatal("Exec did not provide InstanceExecArgs.DataDone")
	}
	select {
	case got := <-resultCh:
		close(allowOutput)
		t.Fatalf("Exec returned before DataDone: %#v", got)
	default:
	}
	close(allowOutput)
	got := <-resultCh
	if got.err != nil {
		t.Fatal(got.err)
	}
	if got.stdout != "flushed stdout" || got.stderr != "flushed stderr" {
		t.Fatalf("Exec output = %q, %q, want flushed output", got.stdout, got.stderr)
	}
}

func TestExecRejectsMissingOrMalformedReturnMetadataWithOutput(t *testing.T) {
	tests := []struct {
		name     string
		metadata map[string]any
	}{
		{name: "missing", metadata: map[string]any{}},
		{name: "wrong type", metadata: map[string]any{"return": "0"}},
		{name: "non-integral", metadata: map[string]any{"return": float64(1.5)}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			call := func(_ api.InstanceExecPost, args *incus.InstanceExecArgs) (operationWaiter, error) {
				_, _ = io.WriteString(args.Stdout, "stdout before metadata")
				_, _ = io.WriteString(args.Stderr, "stderr before metadata")
				if args.DataDone != nil {
					close(args.DataDone)
				}
				return &fakeOperation{operation: api.Operation{Metadata: tt.metadata}}, nil
			}

			stdout, stderr, err := exec(context.Background(), call, ExecRequest{Command: []string{"echo"}})
			if err == nil || !strings.Contains(err.Error(), "return metadata") {
				t.Fatalf("Exec error = %v, want return metadata error", err)
			}
			if stdout != "stdout before metadata" || stderr != "stderr before metadata" {
				t.Fatalf("Exec output = %q, %q, want preserved output", stdout, stderr)
			}
		})
	}
}

func TestExecCancellationSnapshotsConcurrentOutputSafely(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	writerStarted := make(chan struct{})
	writerDone := make(chan struct{})
	operation := &cancelingExecOperation{
		cancel:        cancel,
		writerStarted: writerStarted,
		operation:     api.Operation{Metadata: map[string]any{"return": float64(0)}},
	}
	call := func(_ api.InstanceExecPost, args *incus.InstanceExecArgs) (operationWaiter, error) {
		_, _ = io.WriteString(args.Stdout, "stdout before cancel")
		go func() {
			defer close(writerDone)
			<-ctx.Done()
			close(writerStarted)
			for range 10_000 {
				_, _ = io.WriteString(args.Stdout, " delayed output")
				runtime.Gosched()
			}
		}()
		return operation, nil
	}

	stdout, _, err := exec(ctx, call, ExecRequest{Command: []string{"echo"}})
	<-writerDone
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Exec error = %v, want context cancellation", err)
	}
	if !strings.HasPrefix(stdout, "stdout before cancel") {
		t.Fatalf("Exec stdout = %q, want safely captured prefix", stdout)
	}
}

func TestExecDataFlushHonorsRequestCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	call := func(_ api.InstanceExecPost, args *incus.InstanceExecArgs) (operationWaiter, error) {
		_, _ = io.WriteString(args.Stdout, "stdout before cancel")
		cancel()
		return completedExecOperation(float64(0)), nil
	}

	stdout, _, err := exec(ctx, call, ExecRequest{Command: []string{"echo"}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Exec error = %v, want context cancellation", err)
	}
	if stdout != "stdout before cancel" {
		t.Fatalf("Exec stdout = %q, want preserved output", stdout)
	}
}

type cancelableFakeOperation struct {
	*fakeOperation
	cancelled bool
}

func (o *cancelableFakeOperation) Cancel() error {
	o.cancelled = true
	return nil
}

type cancelingExecOperation struct {
	cancel        context.CancelFunc
	writerStarted <-chan struct{}
	operation     api.Operation
}

func (o *cancelingExecOperation) WaitContext(context.Context) error {
	o.cancel()
	<-o.writerStarted
	return nil
}

func (o *cancelingExecOperation) Get() api.Operation {
	return o.operation
}

func completedExecOperation(status float64) *fakeOperation {
	return &fakeOperation{operation: api.Operation{Metadata: map[string]any{"return": status}}}
}
