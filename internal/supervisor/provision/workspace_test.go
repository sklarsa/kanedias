package provision

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/sklarsa/kanedias/internal/config"
	"github.com/sklarsa/kanedias/internal/incusclient"
	"github.com/sklarsa/kanedias/internal/supervisor/contract"
)

type execResult struct {
	stdout string
	stderr string
	err    error
}

type execRecordingClient struct {
	calls   [][]string
	results map[string]execResult
	execErr error
}

func (c *execRecordingClient) Exec(_ context.Context, _ string, request incusclient.ExecRequest) (string, string, error) {
	command := append([]string(nil), request.Command...)
	c.calls = append(c.calls, command)
	if c.execErr != nil {
		return "", "", c.execErr
	}
	result := c.results[strings.Join(command, "\x00")]
	return result.stdout, result.stderr, result.err
}

func commandKey(command []string) string { return strings.Join(command, "\x00") }

func repositoryRootValidationCommands() [][]string {
	return [][]string{
		{"test", "!", "-L", "/workspace/repos"},
		{"test", "!", "-e", "/workspace/repos"},
	}
}

func ownershipCommands() [][]string {
	return append(repositoryRootValidationCommands(),
		[]string{"chown", "kanedias:kanedias", "/workspace"},
		[]string{"chmod", "0755", "/workspace"},
		[]string{"install", "-d", "-o", "kanedias", "-g", "kanedias", "-m", "0755", "/workspace/repos"},
		[]string{"chown", "kanedias:kanedias", "/workspace/repos"},
		[]string{"chmod", "0755", "/workspace/repos"},
	)
}

func checkoutValidationCommands() [][]string {
	return [][]string{
		{"test", "!", "-L", "/workspace/repos/repo"},
		{"test", "-d", "/workspace/repos/repo"},
		{"test", "!", "-L", "/workspace/repos/repo/.git"},
		{"test", "-d", "/workspace/repos/repo/.git"},
		{"realpath", "-e", "--", "/workspace/repos/repo"},
		{"runuser", "-u", "kanedias", "--", "env", "HOME=/home/kanedias", "USER=kanedias", "LOGNAME=kanedias", "git", "-C", "/workspace/repos/repo", "rev-parse", "--show-toplevel"},
	}
}

func validCheckoutClient() *execRecordingClient {
	commands := checkoutValidationCommands()
	return &execRecordingClient{results: map[string]execResult{
		commandKey(commands[4]): {stdout: "/workspace/repos/repo\n"},
		commandKey(commands[5]): {stdout: "/workspace/repos/repo\n"},
	}}
}

func TestPrepareSessionWorkspaceRepairsOwnershipOnlyForDefaultStart(t *testing.T) {
	client := &execRecordingClient{}
	if err := prepareSessionWorkspace(context.Background(), client, "session-test", config.WorkspaceStart{}); err != nil {
		t.Fatal(err)
	}
	if want := ownershipCommands(); !reflect.DeepEqual(client.calls, want) {
		t.Fatalf("exec calls = %#v, want %#v", client.calls, want)
	}
}

func TestPrepareSessionWorkspacePropagatesOwnershipExecFailure(t *testing.T) {
	primary := errors.New("exec failed")
	client := &execRecordingClient{results: map[string]execResult{
		commandKey([]string{"chown", "kanedias:kanedias", "/workspace"}): {err: primary},
	}}
	err := prepareSessionWorkspace(context.Background(), client, "session-test", config.WorkspaceStart{})
	if !errors.Is(err, primary) {
		t.Fatalf("prepareSessionWorkspace error = %v, want wrapped %v", err, primary)
	}
	want := append(repositoryRootValidationCommands(), []string{"chown", "kanedias:kanedias", "/workspace"})
	if !reflect.DeepEqual(client.calls, want) {
		t.Fatalf("failing exec calls = %#v, want %#v", client.calls, want)
	}
}

func TestPrepareSessionWorkspaceRejectsUnsafeRepositoryRootBeforeOwnershipMutation(t *testing.T) {
	for _, start := range []struct {
		name  string
		value config.WorkspaceStart
	}{
		{name: "default", value: config.WorkspaceStart{}},
		{name: "selected", value: config.WorkspaceStart{Repository: "owner/repo", Checkout: "repo"}},
	} {
		for _, state := range []struct {
			name    string
			results map[string]execResult
			want    [][]string
		}{
			{
				name: "symlink",
				results: map[string]execResult{
					commandKey([]string{"test", "!", "-L", "/workspace/repos"}): {err: errors.New("repository root is a symlink to /host/private")},
				},
				want: repositoryRootValidationCommands()[:1],
			},
			{
				name: "non-directory",
				results: map[string]execResult{
					commandKey([]string{"test", "!", "-e", "/workspace/repos"}): {err: errors.New("repository root exists")},
					commandKey([]string{"test", "-d", "/workspace/repos"}):      {err: errors.New("repository root is a regular file")},
				},
				want: append(repositoryRootValidationCommands(), []string{"test", "-d", "/workspace/repos"}),
			},
		} {
			t.Run(start.name+"/"+state.name, func(t *testing.T) {
				client := &execRecordingClient{results: state.results}
				err := prepareSessionWorkspace(context.Background(), client, "session-test", start.value)
				if err == nil || !strings.Contains(err.Error(), "repository root") {
					t.Fatalf("prepareSessionWorkspace error = %v, want internal repository-root diagnostic", err)
				}
				if !reflect.DeepEqual(client.calls, state.want) {
					t.Fatalf("exec calls = %#v, want fail-closed calls %#v", client.calls, state.want)
				}
				for _, call := range client.calls {
					if call[0] == "install" || call[0] == "chown" || call[0] == "chmod" {
						t.Fatalf("unsafe repository root reached ownership mutation: %#v", call)
					}
				}
			})
		}
	}
}

func TestPrepareSessionWorkspaceValidatesSelectedCheckoutWithArgumentArrays(t *testing.T) {
	client := validCheckoutClient()
	start := config.WorkspaceStart{Repository: "owner/repo", Checkout: "repo"}
	if err := prepareSessionWorkspace(context.Background(), client, "session-test", start); err != nil {
		t.Fatal(err)
	}
	want := append(ownershipCommands(), checkoutValidationCommands()...)
	if !reflect.DeepEqual(client.calls, want) {
		t.Fatalf("exec calls = %#v, want %#v", client.calls, want)
	}
	for _, command := range client.calls {
		if len(command) >= 2 && command[0] == "sh" && command[1] == "-c" {
			t.Fatalf("checkout validation used shell command: %#v", command)
		}
	}
}

func TestPrepareSessionWorkspaceRejectsUnavailableSelectedCheckout(t *testing.T) {
	primary := errors.New("controlled exec failure")
	commands := checkoutValidationCommands()
	tests := []struct {
		name       string
		fail       int
		result     execResult
		underlying error
	}{
		{name: "path is symlink", fail: 0, result: execResult{err: primary}, underlying: primary},
		{name: "path missing", fail: 1, result: execResult{err: primary}, underlying: primary},
		{name: "git metadata is symlink", fail: 2, result: execResult{err: primary}, underlying: primary},
		{name: "git metadata missing", fail: 3, result: execResult{err: primary}, underlying: primary},
		{name: "canonical path differs", fail: 4, result: execResult{stdout: "/workspace/repos/other\n"}},
		{name: "git top level differs", fail: 5, result: execResult{stdout: "/workspace/repos\n"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := validCheckoutClient()
			client.results[commandKey(commands[tt.fail])] = tt.result
			err := prepareSessionWorkspace(context.Background(), client, "session-test", config.WorkspaceStart{Repository: "owner/repo", Checkout: "repo"})
			var contractErr *contract.Error
			if !errors.As(err, &contractErr) || contractErr.Code != contract.ErrorWorkspaceRepositoryUnavailable {
				t.Fatalf("error = %v, want %s", err, contract.ErrorWorkspaceRepositoryUnavailable)
			}
			if tt.underlying != nil && !errors.Is(err, tt.underlying) {
				t.Fatalf("error = %v, want underlying %v", err, tt.underlying)
			}
			if tt.underlying == nil && !strings.Contains(err.Error(), "got") {
				t.Fatalf("comparison error = %v, want underlying mismatch detail", err)
			}
			want := append(ownershipCommands(), commands[:tt.fail+1]...)
			if !reflect.DeepEqual(client.calls, want) {
				t.Fatalf("exec calls = %#v, want %#v", client.calls, want)
			}
		})
	}
}
