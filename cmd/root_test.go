package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/sklarsa/kanedias/internal/config"
	"github.com/sklarsa/kanedias/internal/proxy"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func TestCommandHierarchyAndFlags(t *testing.T) {
	root := newRootCommand(stubServices(), testProxyOptions())

	assertChildCommands(t, root, "image", "profile", "proxy", "sandbox", "session", "workspace")
	assertChildCommands(t, mustFindCommand(t, root, "image"), "create")
	assertChildCommands(t, mustFindCommand(t, root, "proxy"), "init-ca", "login", "run")
	assertChildCommands(t, mustFindCommand(t, root, "proxy", "login"), "openai-codex")
	assertChildCommands(t, mustFindCommand(t, root, "sandbox"), "create", "destroy")
	assertChildCommands(t, mustFindCommand(t, root, "workspace"), "sync")

	for _, path := range [][]string{
		{"image"},
		{"image", "create"},
		{"profile"},
		{"proxy"},
		{"proxy", "run"},
		{"proxy", "init-ca"},
		{"proxy", "login"},
		{"proxy", "login", "openai-codex"},
		{"sandbox"},
		{"sandbox", "create"},
		{"sandbox", "destroy"},
		{"session"},
		{"workspace"},
		{"workspace", "sync"},
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
		{"sandbox", "create"},
		{"sandbox", "destroy"},
		{"session"},
		{"workspace", "sync"},
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
}

func TestSessionReadsPromptFromStdinAndDelegates(t *testing.T) {
	cfg := config.Config{BaseImage: config.BaseImage{Name: "sentinel"}}
	ctx := context.WithValue(context.Background(), struct{}{}, "session-context")
	var stdout, stderr bytes.Buffer
	var calls []string

	service := stubServices()
	service.loadConfig = func(path string) (config.Config, error) {
		calls = append(calls, "load")
		if path != "/tmp/session.toml" {
			t.Errorf("loaded path = %q, want /tmp/session.toml", path)
		}
		return cfg, nil
	}
	service.runSession = func(gotContext context.Context, gotConfig config.Config, prompt string, gotStdout, gotStderr io.Writer) error {
		calls = append(calls, "run")
		if gotContext != ctx {
			t.Error("session did not receive the exact command context")
		}
		if !reflect.DeepEqual(gotConfig, cfg) {
			t.Errorf("session config = %#v, want %#v", gotConfig, cfg)
		}
		if prompt != "first line\nsecond line\n" {
			t.Errorf("prompt = %q, want exact stdin", prompt)
		}
		if gotStdout != &stdout {
			t.Error("session did not receive the exact stdout writer")
		}
		if gotStderr != &stderr {
			t.Error("session did not receive the exact stderr writer")
		}
		_, err := io.WriteString(gotStdout, "{\"type\":\"agent_settled\"}\n")
		return err
	}

	root := newRootCommand(service, testProxyOptions())
	root.SetIn(strings.NewReader("first line\nsecond line\n"))
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"--config", "/tmp/session.toml", "session"})
	if err := root.ExecuteContext(ctx); err != nil {
		t.Fatalf("ExecuteContext() error = %v", err)
	}
	if !reflect.DeepEqual(calls, []string{"load", "run"}) {
		t.Errorf("call order = %q, want [load run]", calls)
	}
	if stdout.String() != "{\"type\":\"agent_settled\"}\n" {
		t.Errorf("stdout = %q", stdout.String())
	}
}

func TestSessionRejectsEmptyInputBeforeWorkflow(t *testing.T) {
	for _, input := range []string{"", " \n\t"} {
		t.Run(strings.ReplaceAll(input, "\n", "\\n"), func(t *testing.T) {
			runCalls := 0
			service := stubServices()
			service.loadConfig = func(string) (config.Config, error) {
				t.Fatal("loadConfig called for empty session input")
				return config.Config{}, nil
			}
			service.runSession = func(context.Context, config.Config, string, io.Writer, io.Writer) error {
				runCalls++
				return nil
			}

			root := newRootCommand(service, testProxyOptions())
			root.SetIn(strings.NewReader(input))
			root.SetOut(io.Discard)
			root.SetErr(io.Discard)
			root.SetArgs([]string{"session"})
			if err := root.Execute(); err == nil {
				t.Fatal("Execute() error = nil, want empty-input error")
			}
			if runCalls != 0 {
				t.Errorf("runSession calls = %d, want 0", runCalls)
			}
		})
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
		{name: "sandbox create default", args: []string{"sandbox", "create"}, workflow: "sandbox-create", wantName: "sandbox"},
		{name: "sandbox create named", args: []string{"sandbox", "create", "personal"}, workflow: "sandbox-create", wantName: "personal"},
		{name: "sandbox destroy default", args: []string{"sandbox", "destroy"}, workflow: "sandbox-destroy", wantName: "sandbox"},
		{name: "sandbox destroy named", args: []string{"sandbox", "destroy", "personal"}, workflow: "sandbox-destroy", wantName: "personal"},
		{name: "workspace sync", args: []string{"workspace", "sync"}, workflow: "workspace"},
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
			service.createSandbox = func(ctx context.Context, cfg config.Config, name string, stdout, stderr io.Writer) error {
				return check(ctx, cfg, stdout, stderr, "sandbox-create", name)
			}
			service.destroySandbox = func(ctx context.Context, cfg config.Config, name string, stdout, stderr io.Writer) error {
				return check(ctx, cfg, stdout, stderr, "sandbox-destroy", name)
			}
			service.syncWorkspace = func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer) error {
				return check(ctx, cfg, stdout, stderr, "workspace", "")
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
		{"sandbox", "create"},
		{"sandbox", "destroy", "personal"},
		{"workspace", "sync"},
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
			service.createSandbox = func(context.Context, config.Config, string, io.Writer, io.Writer) error {
				t.Fatal("createSandbox called after config error")
				return nil
			}
			service.destroySandbox = func(context.Context, config.Config, string, io.Writer, io.Writer) error {
				t.Fatal("destroySandbox called after config error")
				return nil
			}
			service.syncWorkspace = func(context.Context, config.Config, io.Writer, io.Writer) error {
				t.Fatal("syncWorkspace called after config error")
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
		{"sandbox", "create", "one", "two"},
		{"sandbox", "destroy", "one", "two"},
		{"session", "extra"},
		{"workspace", "sync", "extra"},
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
		createSandbox: func(context.Context, config.Config, string, io.Writer, io.Writer) error {
			return nil
		},
		destroySandbox: func(context.Context, config.Config, string, io.Writer, io.Writer) error {
			return nil
		},
		runSession: func(context.Context, config.Config, string, io.Writer, io.Writer) error {
			return nil
		},
		syncWorkspace: func(context.Context, config.Config, io.Writer, io.Writer) error {
			return nil
		},
	}
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
		got = append(got, child.Name())
	}
	if !reflect.DeepEqual(got, names) {
		t.Errorf("%s child commands = %q, want %q", command.CommandPath(), got, names)
	}
}
