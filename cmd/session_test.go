package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/sklarsa/kanedias/internal/config"
	"github.com/sklarsa/kanedias/internal/supervisor/contract"
	"github.com/sklarsa/kanedias/internal/supervisor/process"
	"golang.org/x/sys/unix"
)

func TestSessionRequiresSocketAndRunsForegroundSupervisor(t *testing.T) {
	cfg := validSupervisorConfig()
	cfg.BaseImage.Name = "sentinel"
	var output bytes.Buffer
	service := stubServices()
	service.loadConfig = func(path string) (config.Config, error) {
		if path != "/tmp/custom.toml" {
			t.Fatalf("config path = %q", path)
		}
		return cfg, nil
	}
	var gotOptions SessionOptions
	service.runSupervisor = func(ctx context.Context, got config.Config, options SessionOptions, writer io.Writer) error {
		if !reflect.DeepEqual(got, cfg) {
			t.Fatalf("config = %#v", got)
		}
		if writer != &output {
			t.Fatal("output writer was not forwarded")
		}
		gotOptions = options
		return nil
	}
	root := newRootCommand(service, testProxyOptions())
	root.SetOut(&output)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"--config", "/tmp/custom.toml", "session", "--socket", "./root.sock"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	wantConfig, _ := filepath.Abs("/tmp/custom.toml")
	wantPolicy, err := cfg.DefaultSessionModelPolicy()
	if err != nil {
		t.Fatal(err)
	}
	if gotOptions.SocketPath != "./root.sock" || gotOptions.ConfigPath != wantConfig || !reflect.DeepEqual(gotOptions.Policy, wantPolicy) {
		t.Fatalf("options = %#v", gotOptions)
	}
	if gotOptions.Workspace != (config.WorkspaceStart{}) || gotOptions.Workspace.Directory() != config.WorkspaceRoot {
		t.Fatalf("direct CLI workspace = %#v at %q, want zero value at /workspace", gotOptions.Workspace, gotOptions.Workspace.Directory())
	}
}

func TestSessionRejectsMissingSocketBeforeLoadingConfig(t *testing.T) {
	service := stubServices()
	service.loadConfig = func(string) (config.Config, error) {
		t.Fatal("loadConfig called without required --socket")
		return config.Config{}, nil
	}
	root := newRootCommand(service, testProxyOptions())
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"session"})
	if err := root.Execute(); err == nil {
		t.Fatal("session without --socket succeeded")
	}
}

func TestSessionDoesNotReadStdin(t *testing.T) {
	service := stubServices()
	service.loadConfig = func(string) (config.Config, error) { return validSupervisorConfig(), nil }
	service.runSupervisor = func(context.Context, config.Config, SessionOptions, io.Writer) error { return nil }
	root := newRootCommand(service, testProxyOptions())
	root.SetIn(readerThatFails{t: t})
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"session", "--socket", "/tmp/root.sock"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
}

func TestSessionWorkspaceStartInheritanceUsesExactFDAndKeepsStdinUntouched(t *testing.T) {
	cfg := validSupervisorConfig()
	policy, err := cfg.DefaultSessionModelPolicy()
	if err != nil {
		t.Fatal(err)
	}
	policy.Root = config.ModelProfile{Provider: "local-executor", Model: "Qwen3.6-27B-GGUF", ThinkingLevel: "off"}
	workspace := config.WorkspaceStart{Repository: "owner/repo", Checkout: "repo"}
	bootstrapFD := rootBootstrapReadFDForPolicy(t, policy, workspace)

	service := stubServices()
	service.loadConfig = func(string) (config.Config, error) { return cfg, nil }
	var got SessionOptions
	service.runSupervisor = func(_ context.Context, _ config.Config, options SessionOptions, _ io.Writer) error {
		got = options
		return nil
	}
	root := newRootCommand(service, testProxyOptions())
	root.SetIn(readerThatFails{t: t})
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"session", "--socket", "/tmp/root.sock", "--bootstrap-fd", strconv.Itoa(bootstrapFD)})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Policy, policy) {
		t.Fatalf("policy = %#v, want %#v", got.Policy, policy)
	}
	if got.Workspace != workspace {
		t.Fatalf("workspace = %#v, want %#v", got.Workspace, workspace)
	}
	assertDescriptorClosed(t, bootstrapFD)
}

func TestSessionInheritedBootstrapClosesDescriptorWhenSocketValidationFails(t *testing.T) {
	bootstrapFD := rootBootstrapReadFD(t)
	service := stubServices()
	service.loadConfig = func(string) (config.Config, error) {
		t.Fatal("loadConfig called without required --socket")
		return config.Config{}, nil
	}
	root := newRootCommand(service, testProxyOptions())
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"session", "--bootstrap-fd", strconv.Itoa(bootstrapFD)})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "--socket is required") {
		t.Fatalf("Execute error = %v, want socket validation error", err)
	}
	assertDescriptorClosed(t, bootstrapFD)
}

func TestSessionInheritedBootstrapClosesDescriptorWhenConfigLoadFails(t *testing.T) {
	bootstrapFD := rootBootstrapReadFD(t)
	sentinel := errors.New("load config sentinel")
	service := stubServices()
	service.loadConfig = func(string) (config.Config, error) { return config.Config{}, sentinel }
	root := newRootCommand(service, testProxyOptions())
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"session", "--socket", "/tmp/root.sock", "--bootstrap-fd", strconv.Itoa(bootstrapFD)})
	if err := root.Execute(); !errors.Is(err, sentinel) {
		t.Fatalf("Execute error = %v, want %v", err, sentinel)
	}
	assertDescriptorClosed(t, bootstrapFD)
}

func TestSessionInheritedBootstrapClosesDescriptorWhenConfigPathResolutionFails(t *testing.T) {
	bootstrapFD := rootBootstrapReadFD(t)
	originalDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	removedDir, err := os.MkdirTemp("", "kanedias-removed-cwd-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(removedDir); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if restoreErr := os.Chdir(originalDir); restoreErr != nil {
			t.Errorf("restore working directory: %v", restoreErr)
		}
	}()
	if err := os.Remove(removedDir); err != nil {
		t.Fatal(err)
	}

	service := stubServices()
	service.loadConfig = func(string) (config.Config, error) {
		t.Fatal("loadConfig called after config-path resolution failure")
		return config.Config{}, nil
	}
	root := newRootCommand(service, testProxyOptions())
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"--config", "relative.toml", "session", "--socket", "/tmp/root.sock", "--bootstrap-fd", strconv.Itoa(bootstrapFD)})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "resolve config path") {
		t.Fatalf("Execute error = %v, want config path resolution error", err)
	}
	assertDescriptorClosed(t, bootstrapFD)
}

func TestSessionRejectsBootstrapOnStdinWithoutClosingIt(t *testing.T) {
	savedStdin, err := unix.Dup(0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if restoreErr := unix.Dup2(savedStdin, 0); restoreErr != nil {
			t.Errorf("restore stdin: %v", restoreErr)
		}
		_ = unix.Close(savedStdin)
	}()

	bootstrapFD := rootBootstrapReadFD(t)
	if err := unix.Dup2(bootstrapFD, 0); err != nil {
		t.Fatal(err)
	}
	if err := unix.Close(bootstrapFD); err != nil {
		t.Fatal(err)
	}

	loaded := false
	service := stubServices()
	service.loadConfig = func(string) (config.Config, error) {
		loaded = true
		return validSupervisorConfig(), nil
	}
	root := newRootCommand(service, testProxyOptions())
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"session", "--socket", "/tmp/root.sock", "--bootstrap-fd", "0"})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "at least 3") {
		t.Fatalf("Execute error = %v, want descriptor >= 3 rejection", err)
	}
	if loaded {
		t.Fatal("config loaded before bootstrap descriptor validation")
	}
	if _, err := unix.FcntlInt(0, unix.F_GETFD, 0); err != nil {
		t.Fatalf("stdin was closed: %v", err)
	}
}

func rootBootstrapReadFD(t *testing.T) int {
	t.Helper()
	cfg := validSupervisorConfig()
	policy, err := cfg.DefaultSessionModelPolicy()
	if err != nil {
		t.Fatal(err)
	}
	return rootBootstrapReadFDForPolicy(t, policy, config.WorkspaceStart{})
}

func rootBootstrapReadFDForPolicy(t *testing.T, policy config.SessionModelPolicy, workspace config.WorkspaceStart) int {
	t.Helper()
	var descriptors [2]int
	if err := unix.Pipe2(descriptors[:], unix.O_CLOEXEC); err != nil {
		t.Fatal(err)
	}
	writeEnd := os.NewFile(uintptr(descriptors[1]), "test-root-bootstrap-write")
	if err := process.EncodeRootBootstrap(writeEnd, process.RootBootstrap{Policy: policy, Workspace: workspace}); err != nil {
		_ = unix.Close(descriptors[0])
		_ = writeEnd.Close()
		t.Fatal(err)
	}
	if err := writeEnd.Close(); err != nil {
		_ = unix.Close(descriptors[0])
		t.Fatal(err)
	}
	return descriptors[0]
}

func assertDescriptorClosed(t *testing.T, descriptor int) {
	t.Helper()
	if _, err := unix.FcntlInt(uintptr(descriptor), unix.F_GETFD, 0); !errors.Is(err, unix.EBADF) {
		t.Fatalf("descriptor %d remains open: %v", descriptor, err)
	}
}

func TestSessionStartupDescriptorsCloseOnRuntimeError(t *testing.T) {
	bootstrapFD := rootBootstrapReadFD(t)
	statusRead, statusWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	statusFD := int(statusWrite.Fd())
	sentinel := errors.New("runtime sentinel")
	service := stubServices()
	service.loadConfig = func(string) (config.Config, error) { return validSupervisorConfig(), nil }
	service.runSupervisor = func(_ context.Context, _ config.Config, options SessionOptions, _ io.Writer) error {
		if options.RootStatus == nil {
			t.Fatal("root status writer was not forwarded")
		}
		return sentinel
	}
	root := newRootCommand(service, testProxyOptions())
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"session", "--socket", "/tmp/root.sock", "--bootstrap-fd", strconv.Itoa(bootstrapFD), "--status-fd", strconv.Itoa(statusFD)})
	if err := root.Execute(); !errors.Is(err, sentinel) {
		t.Fatalf("Execute error = %v, want %v", err, sentinel)
	}
	assertDescriptorClosed(t, bootstrapFD)
	assertDescriptorClosed(t, statusFD)
	_ = statusRead.Close()
}

func TestSessionStartupDescriptorsCloseOnBootstrapDecodeError(t *testing.T) {
	bootstrapRead, bootstrapWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bootstrapWrite.WriteString(`{"unknown":true}`); err != nil {
		t.Fatal(err)
	}
	_ = bootstrapWrite.Close()
	statusRead, statusWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	bootstrapFD, statusFD := int(bootstrapRead.Fd()), int(statusWrite.Fd())
	root := newRootCommand(stubServices(), testProxyOptions())
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"session", "--socket", "/tmp/root.sock", "--bootstrap-fd", strconv.Itoa(bootstrapFD), "--status-fd", strconv.Itoa(statusFD)})
	if err := root.Execute(); err == nil {
		t.Fatal("invalid bootstrap succeeded")
	}
	assertDescriptorClosed(t, bootstrapFD)
	assertDescriptorClosed(t, statusFD)
	_ = statusRead.Close()
}

func TestSessionStatusDescriptorValidation(t *testing.T) {
	for _, test := range []struct {
		name        string
		bootstrapFD int
		statusFD    int
		want        string
	}{
		{name: "below fd4", bootstrapFD: 8, statusFD: 3, want: "at least 4"},
		{name: "same as bootstrap", bootstrapFD: 8, statusFD: 8, want: "distinct"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := newRootCommand(stubServices(), testProxyOptions())
			root.SetOut(io.Discard)
			root.SetErr(io.Discard)
			root.SetArgs([]string{"session", "--socket", "/tmp/root.sock", "--bootstrap-fd", strconv.Itoa(test.bootstrapFD), "--status-fd", strconv.Itoa(test.statusFD)})
			if err := root.Execute(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Execute error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestSessionBootstrapAndStatusFlagsAreHiddenAndDefaultAbsent(t *testing.T) {
	root := newRootCommand(stubServices(), testProxyOptions())
	command, _, err := root.Find([]string{"session"})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"bootstrap-fd", "status-fd"} {
		flag := command.Flags().Lookup(name)
		if flag == nil || flag.DefValue != "-1" || !flag.Hidden {
			t.Fatalf("--%s = %#v, want hidden default -1", name, flag)
		}
	}
}

func TestProductionChildRequiresAbsoluteConfigBeforeProvisioning(t *testing.T) {
	bootstrap := process.Bootstrap{
		SessionID: "child", ParentID: "parent", RootID: "root", SocketPath: filepath.Join(t.TempDir(), "child.sock"),
		SourceInstance: "parent-instance", SourceVolume: "parent-volume",
		Policy: config.SessionModelPolicy{
			Root:    config.ModelProfile{Provider: "provider", Model: "root-model", ThinkingLevel: "off"},
			Workers: map[string]config.WorkerProfile{"reviewer": {Description: "review", Provider: "provider", Model: "model", ThinkingLevel: "off"}},
		},
		Request: contract.CreateChildRequest{WorkerType: "reviewer", Kind: contract.ChildKindRead, Context: contract.ContextFresh, Task: "review"},
	}
	for _, path := range []string{"", "relative.toml"} {
		t.Run(path, func(t *testing.T) {
			t.Setenv("KANEDIAS_CONFIG", path)
			reporter := process.NewAcknowledgedReporter(context.Background(), io.Discard, io.NopCloser(bytes.NewReader([]byte{process.TerminalAckByte})), bootstrap.SessionID)
			if err := productionChildRunner(context.Background(), bootstrap, reporter); err == nil {
				t.Fatal("production child accepted missing/relative config")
			}
		})
	}
}

type readerThatFails struct{ t *testing.T }

func (r readerThatFails) Read([]byte) (int, error) { r.t.Fatal("session read stdin"); return 0, nil }
