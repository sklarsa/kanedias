package incusworkspace

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sklarsa/kanedias/internal/incusclient"
)

type execResult struct {
	stdout string
	stderr string
	err    error
}

type recordingExecutor struct {
	commands  [][]string
	instances []string
	results   []execResult
	deadlines []bool
}

func (executor *recordingExecutor) Exec(ctx context.Context, instance string, request incusclient.ExecRequest) (string, string, error) {
	executor.commands = append(executor.commands, append([]string(nil), request.Command...))
	executor.instances = append(executor.instances, instance)
	_, bounded := ctx.Deadline()
	executor.deadlines = append(executor.deadlines, bounded)

	if len(executor.results) == 0 {
		return "", "", nil
	}
	result := executor.results[0]
	executor.results = executor.results[1:]
	return result.stdout, result.stderr, result.err
}

func TestVerifyNativeBtrfs(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		wantErr bool
	}{
		{
			name:    "native btrfs pool",
			payload: `{"name":"default","driver":"btrfs","config":{"source":"/var/lib/incus/storage-pools/default"}}`,
		},
		{
			name:    "wrong driver",
			payload: `{"name":"default","driver":"dir","config":{"source":"/var/lib/incus/storage-pools/default"}}`,
			wantErr: true,
		},
		{
			name:    "loop image",
			payload: `{"name":"default","driver":"btrfs","config":{"source":"/var/lib/incus/disks/default.img"}}`,
			wantErr: true,
		},
		{
			name:    "outside storage pools",
			payload: `{"name":"default","driver":"btrfs","config":{"source":"/other/default"}}`,
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			executor := &recordingExecutor{results: []execResult{{stdout: test.payload}}}
			err := VerifyNativeBtrfs(context.Background(), executor, "workspace")
			if (err != nil) != test.wantErr {
				t.Fatalf("VerifyNativeBtrfs() error = %v, wantErr %v", err, test.wantErr)
			}
			assertCommands(t, executor, [][]string{{"incus", "query", "/1.0/storage-pools/default"}})
		})
	}
}

func TestVerifyNativeBtrfsRejectsMalformedJSON(t *testing.T) {
	executor := &recordingExecutor{results: []execResult{{stdout: "{"}}}
	err := VerifyNativeBtrfs(context.Background(), executor, "workspace")
	if err == nil || !strings.Contains(err.Error(), "decode nested Incus storage pool") {
		t.Fatalf("VerifyNativeBtrfs() error = %v, want decode context", err)
	}
}

func TestWaitReady(t *testing.T) {
	executor := &recordingExecutor{}
	if err := WaitReady(context.Background(), executor, "workspace", 60*time.Second); err != nil {
		t.Fatal(err)
	}
	assertCommands(t, executor, [][]string{{"incus", "admin", "waitready", "--timeout", "60"}})
	if !reflect.DeepEqual(executor.instances, []string{"workspace"}) {
		t.Fatalf("instances = %v, want [workspace]", executor.instances)
	}
	if !reflect.DeepEqual(executor.deadlines, []bool{true}) {
		t.Fatalf("bounded contexts = %v, want [true]", executor.deadlines)
	}
}

func TestInitializeUninitializedSeedRunsMinimalInit(t *testing.T) {
	executor := &recordingExecutor{results: []execResult{
		{},
		{stdout: `[]`},
		{},
		{},
		{stdout: `{"name":"default","driver":"btrfs","config":{"source":"/var/lib/incus/storage-pools/default"}}`},
	}}
	if err := initialize(context.Background(), executor, "workspace", true, 60*time.Second); err != nil {
		t.Fatal(err)
	}
	assertCommands(t, executor, [][]string{
		{"incus", "admin", "waitready", "--timeout", "60"},
		{"incus", "query", "/1.0/storage-pools?recursion=1"},
		{"incus", "admin", "init", "--minimal"},
		{"incus", "admin", "waitready", "--timeout", "60"},
		{"incus", "query", "/1.0/storage-pools/default"},
	})
}

func TestInitializeExistingInitializedSeedDoesNotReinitialize(t *testing.T) {
	executor := &recordingExecutor{results: []execResult{
		{},
		{stdout: `[{"name":"default","driver":"btrfs"}]`},
		{stdout: `{"name":"default","driver":"btrfs","config":{"source":"/var/lib/incus/storage-pools/default"}}`},
	}}
	if err := initialize(context.Background(), executor, "workspace", false, 60*time.Second); err != nil {
		t.Fatal(err)
	}
	assertCommands(t, executor, [][]string{
		{"incus", "admin", "waitready", "--timeout", "60"},
		{"incus", "query", "/1.0/storage-pools?recursion=1"},
		{"incus", "query", "/1.0/storage-pools/default"},
	})
}

func TestInitializeDoesNotReinitializeWhenInitializationProbeFails(t *testing.T) {
	probeErr := errors.New("query failed")
	executor := &recordingExecutor{results: []execResult{{}, {stderr: "boom", err: probeErr}}}
	err := initialize(context.Background(), executor, "workspace", false, 60*time.Second)
	if !errors.Is(err, probeErr) {
		t.Fatalf("initialize error = %v, want probe error", err)
	}
	for _, command := range executor.commands {
		if strings.Join(command, " ") == "incus admin init --minimal" {
			t.Fatal("initialized seed was reinitialized after an ambiguous probe failure")
		}
	}
}

func TestSyncImages(t *testing.T) {
	executor := &recordingExecutor{}
	if err := syncImages(context.Background(), executor, "workspace", []string{"images:debian/13", "images:alpine/3.22"}); err != nil {
		t.Fatal(err)
	}
	assertCommands(t, executor, [][]string{
		{"incus", "image", "copy", "images:debian/13", "local:", "--copy-aliases", "--auto-update", "--reuse"},
		{"incus", "image", "copy", "images:alpine/3.22", "local:", "--copy-aliases", "--auto-update", "--reuse"},
	})
}

func TestQuiesce(t *testing.T) {
	executor := &recordingExecutor{results: []execResult{
		{},
		{},
		{stdout: "inactive\n"},
		{stdout: "inactive\n"},
		{stdout: "0\n"},
	}}
	if err := quiesce(context.Background(), executor, "workspace"); err != nil {
		t.Fatal(err)
	}
	assertCommands(t, executor, quiesceCommands())
}

func TestQuiesceRejectsActiveSocket(t *testing.T) {
	executor := quiesceExecutor("active\n", "inactive\n", "0\n")
	if err := quiesce(context.Background(), executor, "workspace"); err == nil {
		t.Fatal("quiesce() succeeded with active socket")
	}
}

func TestQuiesceRejectsActiveService(t *testing.T) {
	executor := quiesceExecutor("inactive\n", "active\n", "0\n")
	if err := quiesce(context.Background(), executor, "workspace"); err == nil {
		t.Fatal("quiesce() succeeded with active service")
	}
}

func TestQuiesceRejectsNonzeroMainPID(t *testing.T) {
	executor := quiesceExecutor("inactive\n", "inactive\n", "123\n")
	if err := quiesce(context.Background(), executor, "workspace"); err == nil {
		t.Fatal("quiesce() succeeded with nonzero service MainPID")
	}
}

func TestInnerOperationsReturnExecutionErrors(t *testing.T) {
	execErr := errors.New("exec failed")
	tests := []struct {
		name string
		run  func(*recordingExecutor) error
	}{
		{
			name: "wait ready",
			run: func(executor *recordingExecutor) error {
				return WaitReady(context.Background(), executor, "workspace", time.Second)
			},
		},
		{
			name: "verify storage",
			run: func(executor *recordingExecutor) error {
				return VerifyNativeBtrfs(context.Background(), executor, "workspace")
			},
		},
		{
			name: "initialize",
			run: func(executor *recordingExecutor) error {
				return initialize(context.Background(), executor, "workspace", true, time.Second)
			},
		},
		{
			name: "sync images",
			run: func(executor *recordingExecutor) error {
				return syncImages(context.Background(), executor, "workspace", []string{"images:debian/13"})
			},
		},
		{
			name: "quiesce",
			run: func(executor *recordingExecutor) error {
				return quiesce(context.Background(), executor, "workspace")
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			executor := &recordingExecutor{results: []execResult{{err: execErr}}}
			if err := test.run(executor); !errors.Is(err, execErr) {
				t.Fatalf("error = %v, want wrapped %v", err, execErr)
			}
		})
	}
}

func quiesceExecutor(socketState, serviceState, mainPID string) *recordingExecutor {
	return &recordingExecutor{results: []execResult{
		{},
		{},
		{stdout: socketState},
		{stdout: serviceState},
		{stdout: mainPID},
	}}
}

func quiesceCommands() [][]string {
	return [][]string{
		{"systemctl", "stop", "incus.socket"},
		{"systemctl", "stop", "incus.service"},
		{"systemctl", "show", "--property=ActiveState", "--value", "incus.socket"},
		{"systemctl", "show", "--property=ActiveState", "--value", "incus.service"},
		{"systemctl", "show", "--property=MainPID", "--value", "incus.service"},
	}
}

func assertCommands(t *testing.T, executor *recordingExecutor, want [][]string) {
	t.Helper()
	if !reflect.DeepEqual(executor.commands, want) {
		t.Fatalf("commands = %#v, want %#v", executor.commands, want)
	}
}
