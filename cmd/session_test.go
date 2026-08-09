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

func TestSessionInheritedRootBootstrapUsesExactFDAndKeepsStdinUntouched(t *testing.T) {
	cfg := validSupervisorConfig()
	policy, err := cfg.DefaultSessionModelPolicy()
	if err != nil {
		t.Fatal(err)
	}
	policy.Root = config.ModelProfile{Provider: "local-executor", Model: "Qwen3.6-27B-GGUF", ThinkingLevel: "off"}
	bootstrapRead, bootstrapWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := process.EncodeRootBootstrap(bootstrapWrite, process.RootBootstrap{Policy: policy}); err != nil {
		t.Fatal(err)
	}
	if err := bootstrapWrite.Close(); err != nil {
		t.Fatal(err)
	}

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
	root.SetArgs([]string{"session", "--socket", "/tmp/root.sock", "--bootstrap-fd", strconv.Itoa(int(bootstrapRead.Fd()))})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Policy, policy) {
		t.Fatalf("policy = %#v, want %#v", got.Policy, policy)
	}
	if _, err := unix.FcntlInt(bootstrapRead.Fd(), unix.F_GETFD, 0); !errors.Is(err, unix.EBADF) {
		t.Fatalf("bootstrap descriptor remains open: %v", err)
	}
}

func TestSessionBootstrapFlagIsHiddenAndDefaultsAbsent(t *testing.T) {
	root := newRootCommand(stubServices(), testProxyOptions())
	command, _, err := root.Find([]string{"session"})
	if err != nil {
		t.Fatal(err)
	}
	flag := command.Flags().Lookup("bootstrap-fd")
	if flag == nil || flag.DefValue != "-1" || !flag.Hidden {
		t.Fatalf("--bootstrap-fd = %#v, want hidden default -1", flag)
	}
}

func TestProductionChildRequiresAbsoluteConfigBeforeProvisioning(t *testing.T) {
	bootstrap := process.Bootstrap{
		SessionID: "child", ParentID: "parent", RootID: "root", SocketPath: filepath.Join(t.TempDir(), "child.sock"),
		SourceInstance: "parent-instance", SourceVolume: "parent-volume",
		Worker:  config.WorkerProfile{Description: "review", Provider: "provider", Model: "model"},
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
