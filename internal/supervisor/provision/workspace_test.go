package provision

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sklarsa/kanedias/internal/config"
	"github.com/sklarsa/kanedias/internal/incusclient"
)

type execRecordingClient struct {
	calls   []string
	execErr error
}

func (c *execRecordingClient) Exec(_ context.Context, _ string, request incusclient.ExecRequest) (string, string, error) {
	c.calls = append(c.calls, "exec "+strings.Join(request.Command, " "))
	return "", "", c.execErr
}

func TestPrepareSessionWorkspaceRepairsOwnership(t *testing.T) {
	client := &execRecordingClient{}
	if err := prepareSessionWorkspace(context.Background(), client, "session-test", config.WorkspaceStart{}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"exec chown kanedias:kanedias /workspace",
		"exec chmod 0755 /workspace",
		"exec install -d -o kanedias -g kanedias -m 0755 /workspace/repos",
		"exec chown kanedias:kanedias /workspace/repos",
		"exec chmod 0755 /workspace/repos",
	}
	if len(client.calls) != len(want) {
		t.Fatalf("exec calls = %#v, want %#v", client.calls, want)
	}
	for i := range want {
		if client.calls[i] != want[i] {
			t.Fatalf("exec call %d = %q, want %q", i, client.calls[i], want[i])
		}
	}
}

func TestPrepareSessionWorkspacePropagatesExecFailure(t *testing.T) {
	primary := errors.New("exec failed")
	client := &execRecordingClient{execErr: primary}
	err := prepareSessionWorkspace(context.Background(), client, "session-test", config.WorkspaceStart{})
	if !errors.Is(err, primary) {
		t.Fatalf("prepareSessionWorkspace error = %v, want wrapped %v", err, primary)
	}
	if len(client.calls) != 1 || client.calls[0] != "exec chown kanedias:kanedias /workspace" {
		t.Fatalf("failing exec calls = %#v, want exactly the first ownership command", client.calls)
	}
}
