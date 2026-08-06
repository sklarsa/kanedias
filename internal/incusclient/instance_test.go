package incusclient

import (
	"context"
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
		return &fakeOperation{}, nil
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
