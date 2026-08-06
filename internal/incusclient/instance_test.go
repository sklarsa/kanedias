package incusclient

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	incus "github.com/lxc/incus/v7/client"
	"github.com/lxc/incus/v7/shared/api"
)

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

func completedExecOperation(status float64) *fakeOperation {
	return &fakeOperation{operation: api.Operation{Metadata: map[string]any{"return": status}}}
}
