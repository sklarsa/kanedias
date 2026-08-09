package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sklarsa/kanedias/internal/config"
	"github.com/sklarsa/kanedias/internal/proxy"
	"github.com/sklarsa/kanedias/internal/server"
	"github.com/sklarsa/kanedias/internal/supervisor/contract"
	"github.com/sklarsa/kanedias/internal/supervisor/process"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func TestCommandHierarchyAndFlags(t *testing.T) {
	root := newRootCommand(stubServices(), testProxyOptions())

	assertChildCommands(t, root, "image", "profile", "proxy", "server", "session", "workspace")
	assertChildCommands(t, mustFindCommand(t, root, "image"), "create")
	assertChildCommands(t, mustFindCommand(t, root, "proxy"), "init-ca", "login", "run")
	assertChildCommands(t, mustFindCommand(t, root, "proxy", "login"), "openai-codex")
	assertChildCommands(t, mustFindCommand(t, root, "workspace"), "repos")
	assertChildCommands(t, mustFindCommand(t, root, "workspace", "repos"), "sync")

	command, remaining, err := root.Find([]string{"workspace", "sync"})
	if err == nil && len(remaining) == 0 && command.Name() == "sync" && command.Runnable() {
		t.Fatal("workspace sync resolved to an executable sync command")
	}

	for _, path := range [][]string{
		{"image"},
		{"image", "create"},
		{"profile"},
		{"proxy"},
		{"proxy", "run"},
		{"proxy", "init-ca"},
		{"proxy", "login"},
		{"proxy", "login", "openai-codex"},
		{"server"},
		{"session"},
		{"workspace"},
		{"workspace", "repos", "sync"},
	} {
		command, remaining, err := root.Find(path)
		if err != nil {
			t.Fatalf("find %q: %v", strings.Join(path, " "), err)
		}
		if len(remaining) != 0 {
			t.Fatalf("find %q left arguments %q", strings.Join(path, " "), remaining)
		}
		if command.Name() != path[len(path)-1] {
			t.Errorf("find %q returned %q", strings.Join(path, " "), command.Name())
		}
	}

	run := mustFindCommand(t, root, "proxy", "run")
	if run.Flags().Lookup("listen") != nil {
		t.Error("proxy run unexpectedly exposes a listen flag")
	}
	assertFlags(t, run, "metrics-listen", "request-log", "ca-cert", "ca-key", "claude-credentials", "openai-codex-auth")
	assertFlags(t, mustFindCommand(t, root, "proxy", "init-ca"), "ca-cert", "ca-key")
	assertFlags(t, mustFindCommand(t, root, "proxy", "login", "openai-codex"), "openai-codex-auth")

	serverCommand := mustFindCommand(t, root, "server")
	var serverFlags []string
	serverCommand.LocalNonPersistentFlags().VisitAll(func(flag *pflag.Flag) {
		serverFlags = append(serverFlags, flag.Name)
	})
	if !reflect.DeepEqual(serverFlags, []string{"listen"}) {
		t.Errorf("server local flags = %q, want [listen]", serverFlags)
	}
	listenFlag := serverCommand.Flags().Lookup("listen")
	if listenFlag == nil {
		t.Fatal("server listen flag is missing")
	}
	if listenFlag.DefValue != server.DefaultListenAddress {
		t.Errorf("server listen default = %q, want %q", listenFlag.DefValue, server.DefaultListenAddress)
	}
	if !strings.Contains(listenFlag.Usage, "bind") {
		t.Errorf("server listen usage = %q, want bind-address wording", listenFlag.Usage)
	}

	configFlag := root.PersistentFlags().Lookup("config")
	if configFlag == nil {
		t.Fatal("root config flag is missing")
	}
	if configFlag.DefValue != "./config.toml" {
		t.Errorf("config default = %q, want %q", configFlag.DefValue, "./config.toml")
	}
	if root.CompletionOptions.DisableDefaultCmd {
		t.Error("default completion command is disabled")
	}
	if root.DisableAutoGenTag {
		t.Error("Cobra generated-command behavior is disabled")
	}
	if !strings.Contains(root.Short, "Incus lifecycle management") {
		t.Errorf("root short help = %q, want Incus lifecycle management", root.Short)
	}

	for _, path := range [][]string{
		{"image", "create"},
		{"workspace", "repos", "sync"},
	} {
		command := mustFindCommand(t, root, path...)
		var localFlags []string
		command.LocalNonPersistentFlags().VisitAll(func(flag *pflag.Flag) {
			localFlags = append(localFlags, flag.Name)
		})
		if len(localFlags) != 0 {
			t.Errorf("%s local flags = %q, want none", command.CommandPath(), localFlags)
		}
	}
	assertFlags(t, mustFindCommand(t, root, "session"), "socket")
}

func TestWorkspaceParentShowsHelpAndRejectsLegacySync(t *testing.T) {
	service := stubServices()
	service.loadConfig = func(string) (config.Config, error) {
		t.Fatal("loadConfig called by workspace parent command")
		return config.Config{}, nil
	}
	service.syncRepos = func(context.Context, config.Config, io.Writer, io.Writer) error {
		t.Fatal("syncRepos called by workspace parent command")
		return nil
	}

	t.Run("bare workspace shows help", func(t *testing.T) {
		var stdout bytes.Buffer
		root := newRootCommand(service, testProxyOptions())
		root.SetOut(&stdout)
		root.SetErr(io.Discard)
		root.SetArgs([]string{"workspace"})

		if err := root.Execute(); err != nil {
			t.Fatalf("Execute() error = %v, want successful help", err)
		}
		if !strings.Contains(stdout.String(), "Manage the Incus workspace") {
			t.Errorf("stdout = %q, want workspace help", stdout.String())
		}
	})

	t.Run("legacy sync is rejected", func(t *testing.T) {
		root := newRootCommand(service, testProxyOptions())
		root.SetOut(io.Discard)
		root.SetErr(io.Discard)
		root.SetArgs([]string{"workspace", "sync"})

		if err := root.Execute(); err == nil {
			t.Fatal("Execute() error = nil, want workspace sync argument error")
		}
	})
}

func TestSessionChildCommandIsHiddenAndUsesFixedDescriptorFlags(t *testing.T) {
	root := newRootCommand(stubServices(), testProxyOptions())
	child, remaining, err := root.Find([]string{"session-child"})
	if err != nil || len(remaining) != 0 {
		t.Fatalf("find session-child = (%v, %q, %v)", child, remaining, err)
	}
	if !child.Hidden {
		t.Fatal("session-child command is visible")
	}
	for name, want := range map[string]string{"bootstrap-fd": "3", "liveness-fd": "4", "report-fd": "5", "terminal-ack-fd": "6"} {
		flag := child.Flags().Lookup(name)
		if flag == nil || flag.DefValue != want {
			t.Errorf("--%s = %#v, want default %s", name, flag, want)
		}
	}

	service := stubServices()
	service.runSessionChild = func(context.Context, process.Bootstrap, *process.Reporter) error {
		t.Fatal("runSessionChild called with remapped descriptors")
		return nil
	}
	root = newRootCommand(service, testProxyOptions())
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"session-child", "--bootstrap-fd", "6"})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "fixed descriptors") {
		t.Fatalf("remapped descriptor error = %v", err)
	}
}

func TestHiddenSessionChildMarksProtocolDescriptorsCloseOnExec(t *testing.T) {
	if os.Getenv("KANEDIAS_CLOEXEC_HELPER") == "1" {
		service := stubServices()
		service.runSessionChild = func(context.Context, process.Bootstrap, *process.Reporter) error {
			return syscall.Exec("/bin/sh", []string{"sh", "-c", `for fd in 3 4 5 6; do [ ! -e /proc/self/fd/$fd ] || exit $fd; done`}, os.Environ())
		}
		root := newRootCommand(service, testProxyOptions())
		root.SetArgs([]string{"session-child", "--bootstrap-fd", "3", "--liveness-fd", "4", "--report-fd", "5"})
		if err := root.Execute(); err != nil {
			t.Fatal(err)
		}
		return
	}

	bootstrapRead, bootstrapWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	livenessRead, livenessWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	reportRead, reportWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	ackRead, ackWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	bootstrap := process.Bootstrap{
		SessionID: "child-1", ParentID: "parent-1", RootID: "root-1",
		SocketPath: filepath.Join(t.TempDir(), "child.sock"), SourceInstance: "instance", SourceVolume: "volume",
		Worker:  config.WorkerProfile{Description: "review", Provider: "provider", Model: "model"},
		Request: contract.CreateChildRequest{WorkerType: "reviewer", Kind: contract.ChildKindRead, Context: contract.ContextFresh, Task: "review"},
	}
	command := exec.Command(os.Args[0], "-test.run=TestHiddenSessionChildMarksProtocolDescriptorsCloseOnExec", "--")
	command.Env = append(os.Environ(), "KANEDIAS_CLOEXEC_HELPER=1")
	command.ExtraFiles = []*os.File{bootstrapRead, livenessRead, reportWrite, ackRead}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	_ = bootstrapRead.Close()
	_ = livenessRead.Close()
	_ = reportWrite.Close()
	_ = ackRead.Close()
	if err := process.EncodeBootstrap(bootstrapWrite, bootstrap); err != nil {
		t.Fatal(err)
	}
	_ = bootstrapWrite.Close()

	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("hidden command CLOEXEC helper: %v: %s", err, stderr.String())
		}
	case <-time.After(3 * time.Second):
		_ = command.Process.Kill()
		t.Fatal("hidden command did not exec descriptor-check helper")
	}
	_ = livenessWrite.Close()
	_ = reportRead.Close()
	_ = ackWrite.Close()
}

func TestServerCommandRejectsPositionalArguments(t *testing.T) {
	service := serverServicesThatRejectDependencies(t)
	service.runServer = func(context.Context, config.Config, server.Options) error {
		t.Fatal("runServer called with positional arguments")
		return nil
	}

	root := newRootCommand(service, testProxyOptions())
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"server", "extra"})
	if err := root.Execute(); err == nil {
		t.Fatal("Execute() succeeded with a positional argument")
	}
}

func TestServerCommandRejectsInvalidListenAddressBeforeDelegation(t *testing.T) {
	for _, address := range []string{":8080", "example.com:8080"} {
		t.Run(address, func(t *testing.T) {
			service := serverServicesThatRejectDependencies(t)
			service.runServer = func(context.Context, config.Config, server.Options) error {
				t.Fatal("runServer called with an invalid listen address")
				return nil
			}

			root := newRootCommand(service, testProxyOptions())
			root.SetOut(io.Discard)
			root.SetErr(io.Discard)
			root.SetArgs([]string{"server", "--listen", address})
			err := root.Execute()
			if err == nil {
				t.Fatal("Execute() succeeded with an unsafe listen address")
			}
			if !strings.Contains(err.Error(), "validate listen address") {
				t.Errorf("Execute() error = %q, want listen validation error", err)
			}
		})
	}
}

type serverCommandTestCtxKey struct{}

func TestServerCommandDelegates(t *testing.T) {
	ctx := context.WithValue(context.Background(), serverCommandTestCtxKey{}, "server context")
	runErr := errors.New("run server sentinel")
	var stderr bytes.Buffer
	calls := 0
	loadCalls := 0

	configDir := t.TempDir()
	configFile := filepath.Join(configDir, "test.toml")
	if err := os.WriteFile(configFile, []byte("[network]\nipv4 = \"10.0.0.1/24\"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	service := serverServicesThatRejectDependencies(t)
	service.loadConfig = func(path string) (config.Config, error) {
		loadCalls++
		absWant, _ := filepath.Abs(configFile)
		if path != absWant {
			t.Errorf("loadConfig path = %q, want absolute %q", path, absWant)
		}
		return config.Config{Network: config.Network{IPv4: "10.0.0.1/24"}}, nil
	}
	service.runServer = func(gotContext context.Context, gotConfig config.Config, options server.Options) error {
		calls++
		if gotContext != ctx {
			t.Error("runServer did not receive the exact command context")
		}
		if options.ListenAddress != "0.0.0.0:9090" {
			t.Errorf("listen address = %q, want 0.0.0.0:9090", options.ListenAddress)
		}
		if options.Logger == nil {
			t.Fatal("runServer received a nil logger")
		}
		if options.BootstrapOutput == nil {
			t.Fatal("runServer received nil BootstrapOutput")
		}
		if options.ConfigPath == "" {
			t.Fatal("runServer received empty ConfigPath")
		}
		options.Logger.Info("server command test", "answer", 42)
		return runErr
	}

	root := newRootCommand(service, testProxyOptions())
	root.SetContext(ctx)
	root.SetOut(io.Discard)
	root.SetErr(&stderr)
	root.SetArgs([]string{"--config", configFile, "server", "--listen", "0.0.0.0:9090"})
	if err := root.Execute(); !errors.Is(err, runErr) {
		t.Fatalf("Execute() error = %v, want run server sentinel", err)
	}
	if calls != 1 {
		t.Errorf("runServer calls = %d, want 1", calls)
	}
	if loadCalls != 1 {
		t.Errorf("loadConfig calls = %d, want 1", loadCalls)
	}
	logOutput := stderr.String()
	if !strings.Contains(logOutput, "msg=\"server command test\"") || !strings.Contains(logOutput, "answer=42") {
		t.Errorf("command stderr = %q, want structured logger output", logOutput)
	}
}

func TestServerCommandUsesDefaultListenAddress(t *testing.T) {
	service := serverServicesThatRejectDependencies(t)
	calls := 0
	service.loadConfig = func(string) (config.Config, error) {
		return config.Config{Network: config.Network{IPv4: "10.0.0.1/24"}}, nil
	}
	service.runServer = func(_ context.Context, _ config.Config, options server.Options) error {
		calls++
		if options.ListenAddress != server.DefaultListenAddress {
			t.Errorf("listen address = %q, want %q", options.ListenAddress, server.DefaultListenAddress)
		}
		return nil
	}

	root := newRootCommand(service, testProxyOptions())
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"server"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if calls != 1 {
		t.Errorf("runServer calls = %d, want 1", calls)
	}
}

func TestExecuteContextPropagatesCancellationToServer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	service := serverServicesThatRejectDependencies(t)
	calls := 0
	service.loadConfig = func(string) (config.Config, error) {
		return config.Config{Network: config.Network{IPv4: "10.0.0.1/24"}}, nil
	}
	service.runServer = func(gotContext context.Context, _ config.Config, _ server.Options) error {
		calls++
		if gotContext != ctx {
			t.Error("runServer did not receive the exact execute context")
		}
		if !errors.Is(gotContext.Err(), context.Canceled) {
			t.Errorf("runServer context error = %v, want context.Canceled", gotContext.Err())
		}
		return gotContext.Err()
	}

	oldArgs := os.Args
	os.Args = []string{"kanedias", "server"}
	t.Cleanup(func() { os.Args = oldArgs })

	if err := execute(ctx, service, testProxyOptions()); !errors.Is(err, context.Canceled) {
		t.Fatalf("execute() error = %v, want context.Canceled", err)
	}
	if calls != 1 {
		t.Errorf("runServer calls = %d, want 1", calls)
	}
}

func TestLifecycleCommandDelegation(t *testing.T) {
	cfg := config.Config{Network: config.Network{IPv4: "10.76.111.1/24"}}
	workflowErr := errors.New("workflow failed")
	tests := []struct {
		name     string
		args     []string
		workflow string
		wantName string
	}{
		{name: "image create", args: []string{"image", "create"}, workflow: "image"},
		{name: "workspace repos sync", args: []string{"workspace", "repos", "sync"}, workflow: "workspace-repos"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			var stdout, stderr bytes.Buffer
			var calls []string

			service := stubServices()
			service.loadConfig = func(path string) (config.Config, error) {
				calls = append(calls, "load")
				if path != "/tmp/lifecycle.toml" {
					t.Errorf("loaded path = %q, want /tmp/lifecycle.toml", path)
				}
				return cfg, nil
			}
			check := func(gotContext context.Context, gotConfig config.Config, gotStdout, gotStderr io.Writer, workflow, name string) error {
				calls = append(calls, workflow)
				if gotContext != ctx {
					t.Error("workflow did not receive the exact command context")
				}
				if !reflect.DeepEqual(gotConfig, cfg) {
					t.Errorf("workflow config = %#v, want %#v", gotConfig, cfg)
				}
				if gotStdout != &stdout {
					t.Error("workflow did not receive the exact stdout writer")
				}
				if gotStderr != &stderr {
					t.Error("workflow did not receive the exact stderr writer")
				}
				if workflow != tt.workflow {
					t.Errorf("workflow = %q, want %q", workflow, tt.workflow)
				}
				if name != tt.wantName {
					t.Errorf("sandbox name = %q, want %q", name, tt.wantName)
				}
				return workflowErr
			}
			service.createImage = func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer) error {
				return check(ctx, cfg, stdout, stderr, "image", "")
			}
			service.syncRepos = func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer) error {
				return check(ctx, cfg, stdout, stderr, "workspace-repos", "")
			}

			root := newRootCommand(service, testProxyOptions())
			root.SetContext(ctx)
			root.SetOut(&stdout)
			root.SetErr(&stderr)
			root.SetArgs(append([]string{"--config", "/tmp/lifecycle.toml"}, tt.args...))
			if err := root.Execute(); !errors.Is(err, workflowErr) {
				t.Fatalf("Execute() error = %v, want workflow error", err)
			}
			if !reflect.DeepEqual(calls, []string{"load", tt.workflow}) {
				t.Errorf("call order = %q, want [load %s]", calls, tt.workflow)
			}
		})
	}
}

func TestLifecycleCommandsStopWhenConfigLoadFails(t *testing.T) {
	loadErr := errors.New("load failed")
	for _, args := range [][]string{
		{"image", "create"},
		{"workspace", "repos", "sync"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			calls := 0
			service := stubServices()
			service.loadConfig = func(string) (config.Config, error) {
				calls++
				return config.Config{}, loadErr
			}
			service.createImage = func(context.Context, config.Config, io.Writer, io.Writer) error {
				t.Fatal("createImage called after config error")
				return nil
			}
			service.syncRepos = func(context.Context, config.Config, io.Writer, io.Writer) error {
				t.Fatal("syncRepos called after config error")
				return nil
			}

			root := newRootCommand(service, testProxyOptions())
			root.SetOut(io.Discard)
			root.SetErr(io.Discard)
			root.SetArgs(args)
			if err := root.Execute(); !errors.Is(err, loadErr) {
				t.Fatalf("Execute() error = %v, want load error", err)
			}
			if calls != 1 {
				t.Errorf("loadConfig calls = %d, want 1", calls)
			}
		})
	}
}

func TestLifecycleCommandsRejectExtraArguments(t *testing.T) {
	for _, args := range [][]string{
		{"image", "create", "extra"},
		{"session", "extra"},
		{"workspace", "repos", "sync", "extra"},
	} {
		root := newRootCommand(stubServices(), testProxyOptions())
		root.SetOut(io.Discard)
		root.SetErr(io.Discard)
		root.SetArgs(args)
		if err := root.Execute(); err == nil {
			t.Errorf("Execute(%q) succeeded, want argument error", args)
		}
	}
}

func TestProfileRejectsMissingAndUnsupportedArguments(t *testing.T) {
	for _, args := range [][]string{{"profile"}, {"profile", "unsupported"}} {
		root := newRootCommand(stubServices(), testProxyOptions())
		root.SetArgs(args)
		root.SetOut(io.Discard)
		root.SetErr(io.Discard)
		if err := root.Execute(); err == nil {
			t.Errorf("Execute(%q) succeeded, want argument error", args)
		}
	}
}

func TestProfileOrchestration(t *testing.T) {
	cfg := config.Config{Network: config.Network{IPv4: "10.76.111.1/24"}}
	var loadedPath string
	var renderedName string
	var renderedConfig config.Config
	service := stubServices()
	service.loadConfig = func(path string) (config.Config, error) {
		loadedPath = path
		return cfg, nil
	}
	service.renderProfile = func(w io.Writer, name string, got config.Config) error {
		renderedName = name
		renderedConfig = got
		_, err := io.WriteString(w, "rendered profile\n")
		return err
	}

	root := newRootCommand(service, testProxyOptions())
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"--config", "/tmp/custom.toml", "profile", "sandbox"})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute profile: %v", err)
	}

	if loadedPath != "/tmp/custom.toml" {
		t.Errorf("loaded path = %q, want /tmp/custom.toml", loadedPath)
	}
	if renderedName != "sandbox" {
		t.Errorf("rendered name = %q, want sandbox", renderedName)
	}
	if !reflect.DeepEqual(renderedConfig, cfg) {
		t.Errorf("rendered config = %#v, want %#v", renderedConfig, cfg)
	}
	if stdout.String() != "rendered profile\n" {
		t.Errorf("stdout = %q, want rendered profile", stdout.String())
	}
}

func TestProxyRunOrchestration(t *testing.T) {
	cfg := config.Config{Network: config.Network{IPv4: "10.76.111.1/24"}}
	var calls []string
	var gotOptions proxy.Options
	service := stubServices()
	service.loadConfig = func(path string) (config.Config, error) {
		calls = append(calls, "load")
		if path != "./config.toml" {
			t.Errorf("loaded path = %q, want ./config.toml", path)
		}
		return cfg, nil
	}
	service.ensureNetwork = func(_ context.Context, got config.Config) error {
		calls = append(calls, "ensure")
		if !reflect.DeepEqual(got, cfg) {
			t.Errorf("ensure config = %#v, want %#v", got, cfg)
		}
		return nil
	}
	type proxyContextKey struct{}
	ctx := context.WithValue(context.Background(), proxyContextKey{}, "proxy-context")
	service.runProxy = func(gotContext context.Context, options proxy.Options) error {
		calls = append(calls, "run")
		if got := gotContext.Value(proxyContextKey{}); got != "proxy-context" {
			t.Errorf("proxy context value = %v, want proxy-context", got)
		}
		gotOptions = options
		return nil
	}

	root := newRootCommand(service, testProxyOptions())
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"proxy", "run", "--request-log", "--metrics-listen", "127.0.0.1:9090"})
	if err := root.ExecuteContext(ctx); err != nil {
		t.Fatalf("execute proxy run: %v", err)
	}

	if !reflect.DeepEqual(calls, []string{"load", "ensure", "run"}) {
		t.Errorf("call order = %q, want [load ensure run]", calls)
	}
	want := testProxyOptions()
	want.ListenAddress = "10.76.111.1:3128"
	want.RequestLog = true
	want.MetricsListenAddress = "127.0.0.1:9090"
	if !reflect.DeepEqual(gotOptions, want) {
		t.Errorf("run options = %#v, want %#v", gotOptions, want)
	}
}

func TestProxyInitCADoesNotLoadConfigOrEnsureNetwork(t *testing.T) {
	service := servicesThatRejectConfigAndNetwork(t)
	var certPath, keyPath string
	service.initCA = func(cert, key string) error {
		certPath, keyPath = cert, key
		return nil
	}

	root := newRootCommand(service, testProxyOptions())
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"proxy", "init-ca", "--ca-cert", "/tmp/custom.crt", "--ca-key", "/tmp/custom.key"})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute proxy init-ca: %v", err)
	}
	if certPath != "/tmp/custom.crt" || keyPath != "/tmp/custom.key" {
		t.Errorf("init paths = (%q, %q), want explicit paths", certPath, keyPath)
	}
}

func TestProxyLoginDoesNotLoadConfigOrEnsureNetwork(t *testing.T) {
	service := servicesThatRejectConfigAndNetwork(t)
	var authPath string
	var gotWriter io.Writer
	service.loginOpenAICodex = func(_ context.Context, path string, writer io.Writer) error {
		authPath = path
		gotWriter = writer
		return nil
	}

	root := newRootCommand(service, testProxyOptions())
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"proxy", "login", "openai-codex", "--openai-codex-auth", "/tmp/custom-auth.json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute proxy login openai-codex: %v", err)
	}
	if authPath != "/tmp/custom-auth.json" {
		t.Errorf("auth path = %q, want explicit path", authPath)
	}
	if gotWriter != &stdout {
		t.Error("login did not receive the Cobra command output writer")
	}
}

func stubServices() services {
	return services{
		loadConfig: func(string) (config.Config, error) {
			return config.Config{Network: config.Network{IPv4: "10.76.111.1/24"}}, nil
		},
		ensureNetwork: func(context.Context, config.Config) error { return nil },
		renderProfile: func(io.Writer, string, config.Config) error { return nil },
		runProxy:      func(context.Context, proxy.Options) error { return nil },
		initCA:        func(string, string) error { return nil },
		loginOpenAICodex: func(context.Context, string, io.Writer) error {
			return nil
		},
		createImage: func(context.Context, config.Config, io.Writer, io.Writer) error {
			return nil
		},
		runSupervisor: func(context.Context, config.Config, SessionOptions, io.Writer) error {
			return nil
		},
		syncRepos: func(context.Context, config.Config, io.Writer, io.Writer) error {
			return nil
		},
		runServer: func(context.Context, config.Config, server.Options) error { return nil },
		runSessionChild: func(context.Context, process.Bootstrap, *process.Reporter) error {
			return nil
		},
	}
}

func serverServicesThatRejectDependencies(t *testing.T) services {
	t.Helper()
	service := stubServices()
	service.loadConfig = func(string) (config.Config, error) {
		t.Fatal("loadConfig called by server command")
		return config.Config{}, nil
	}
	service.ensureNetwork = func(context.Context, config.Config) error {
		t.Fatal("ensureNetwork called by server command")
		return nil
	}
	service.renderProfile = func(io.Writer, string, config.Config) error {
		t.Fatal("renderProfile called by server command")
		return nil
	}
	service.runProxy = func(context.Context, proxy.Options) error {
		t.Fatal("runProxy called by server command")
		return nil
	}
	service.initCA = func(string, string) error {
		t.Fatal("initCA called by server command")
		return nil
	}
	service.loginOpenAICodex = func(context.Context, string, io.Writer) error {
		t.Fatal("loginOpenAICodex called by server command")
		return nil
	}
	service.createImage = func(context.Context, config.Config, io.Writer, io.Writer) error {
		t.Fatal("createImage called by server command")
		return nil
	}
	service.runSupervisor = func(context.Context, config.Config, SessionOptions, io.Writer) error {
		t.Fatal("runSupervisor called by server command")
		return nil
	}
	service.syncRepos = func(context.Context, config.Config, io.Writer, io.Writer) error {
		t.Fatal("syncRepos called by server command")
		return nil
	}
	return service
}

func servicesThatRejectConfigAndNetwork(t *testing.T) services {
	t.Helper()
	service := stubServices()
	service.loadConfig = func(string) (config.Config, error) {
		t.Fatal("loadConfig called for non-listening proxy operation")
		return config.Config{}, nil
	}
	service.ensureNetwork = func(context.Context, config.Config) error {
		t.Fatal("ensureNetwork called for non-listening proxy operation")
		return nil
	}
	return service
}

func testProxyOptions() proxy.Options {
	return proxy.Options{
		ListenAddress:         "127.0.0.1:3128",
		CACertPath:            "/defaults/ca.crt",
		CAKeyPath:             "/defaults/ca.key",
		ClaudeCredentialsPath: "/defaults/claude.json",
		OpenAICodexAuthPath:   "/defaults/openai.json",
	}
}

func mustFindCommand(t *testing.T, root *cobra.Command, path ...string) *cobra.Command {
	t.Helper()
	command, remaining, err := root.Find(path)
	if err != nil {
		t.Fatalf("find %q: %v", strings.Join(path, " "), err)
	}
	if len(remaining) != 0 {
		t.Fatalf("find %q left arguments %q", strings.Join(path, " "), remaining)
	}
	return command
}

func assertFlags(t *testing.T, command *cobra.Command, names ...string) {
	t.Helper()
	for _, name := range names {
		if command.Flags().Lookup(name) == nil {
			t.Errorf("%s is missing --%s", command.CommandPath(), name)
		}
	}
}

func assertChildCommands(t *testing.T, command *cobra.Command, names ...string) {
	t.Helper()
	var got []string
	for _, child := range command.Commands() {
		if !child.Hidden {
			got = append(got, child.Name())
		}
	}
	if !reflect.DeepEqual(got, names) {
		t.Errorf("%s child commands = %q, want %q", command.CommandPath(), got, names)
	}
}
