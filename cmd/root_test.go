package cmd

import (
	"bytes"
	"context"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/sklarsa/kanedias/internal/config"
	"github.com/sklarsa/kanedias/internal/proxy"
	"github.com/spf13/cobra"
)

func TestCommandHierarchyAndFlags(t *testing.T) {
	root := newRootCommand(stubServices(), testProxyOptions())

	assertChildCommands(t, root, "profile", "proxy")
	assertChildCommands(t, mustFindCommand(t, root, "proxy"), "init-ca", "login", "run")
	assertChildCommands(t, mustFindCommand(t, root, "proxy", "login"), "openai-codex")

	for _, path := range [][]string{
		{"profile"},
		{"proxy"},
		{"proxy", "run"},
		{"proxy", "init-ca"},
		{"proxy", "login"},
		{"proxy", "login", "openai-codex"},
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
	service.runProxy = func(options proxy.Options) error {
		calls = append(calls, "run")
		gotOptions = options
		return nil
	}

	root := newRootCommand(service, testProxyOptions())
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"proxy", "run", "--request-log", "--metrics-listen", "127.0.0.1:9090"})
	if err := root.Execute(); err != nil {
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
		runProxy:      func(proxy.Options) error { return nil },
		initCA:        func(string, string) error { return nil },
		loginOpenAICodex: func(context.Context, string, io.Writer) error {
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
