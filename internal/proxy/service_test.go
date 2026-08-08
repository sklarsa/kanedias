package proxy

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultOptions(t *testing.T) {
	homeDir := t.TempDir()
	configDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("XDG_CONFIG_HOME", configDir)

	options, err := DefaultOptions()
	if err != nil {
		t.Fatal(err)
	}
	if options.ListenAddress != "127.0.0.1:3128" {
		t.Errorf("ListenAddress = %q, want %q", options.ListenAddress, "127.0.0.1:3128")
	}
	if options.MetricsListenAddress != "" {
		t.Errorf("MetricsListenAddress = %q, want empty", options.MetricsListenAddress)
	}
	if options.RequestLog {
		t.Error("RequestLog = true, want false")
	}
	if options.CACertPath != filepath.Join(configDir, "kanedias-proxy", "ca.crt") {
		t.Errorf("CACertPath = %q", options.CACertPath)
	}
	if options.CAKeyPath != filepath.Join(configDir, "kanedias-proxy", "ca.key") {
		t.Errorf("CAKeyPath = %q", options.CAKeyPath)
	}
	if options.ClaudeCredentialsPath != filepath.Join(homeDir, ".claude", ".credentials.json") {
		t.Errorf("ClaudeCredentialsPath = %q", options.ClaudeCredentialsPath)
	}
	if options.OpenAICodexAuthPath != filepath.Join(configDir, "kanedias-proxy", "openai-codex.json") {
		t.Errorf("OpenAICodexAuthPath = %q", options.OpenAICodexAuthPath)
	}
}

func TestInitCA(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.crt")
	keyPath := filepath.Join(dir, "ca.key")

	if err := InitCA(certPath, keyPath); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		path string
		mode os.FileMode
	}{
		{path: certPath, mode: 0644},
		{path: keyPath, mode: 0600},
	} {
		info, err := os.Stat(test.path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != test.mode {
			t.Errorf("%s mode = %04o, want %04o", test.path, info.Mode().Perm(), test.mode)
		}
	}
	if err := InitCA(certPath, keyPath); err != nil {
		t.Fatalf("second InitCA call: %v", err)
	}
}

func TestLoginOpenAICodexHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := LoginOpenAICodex(ctx, filepath.Join(t.TempDir(), "openai-codex.json"), io.Discard)
	if err == nil {
		t.Fatal("LoginOpenAICodex returned nil error for canceled context")
	}
}

func TestRunRejectsEmptyListenAddress(t *testing.T) {
	err := Run(Options{})
	if err == nil {
		t.Fatal("Run returned nil error for empty listen address")
	}
}

func TestRunContextTreatsCancellationAsCleanShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dir := t.TempDir()

	err := RunContext(ctx, Options{
		ListenAddress: "127.0.0.1:0",
		CACertPath:    filepath.Join(dir, "ca.crt"),
		CAKeyPath:     filepath.Join(dir, "ca.key"),
	})
	if err != nil {
		t.Fatalf("RunContext() error = %v, want clean shutdown", err)
	}
}

func TestRunContextShutsDownSiblingOnServerError(t *testing.T) {
	dir := t.TempDir()

	// Reserve then free a proxy port so RunContext can bind it.
	proxyL, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxyAddr := proxyL.Addr().String()
	if err := proxyL.Close(); err != nil {
		t.Fatal(err)
	}

	// Occupy a metrics port so the metrics server fails to bind.
	metricsL, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = metricsL.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err = RunContext(ctx, Options{
		ListenAddress:        proxyAddr,
		MetricsListenAddress: metricsL.Addr().String(),
		CACertPath:           filepath.Join(dir, "ca.crt"),
		CAKeyPath:            filepath.Join(dir, "ca.key"),
	})
	if err == nil {
		t.Fatal("RunContext() = nil, want error when metrics listener fails to bind")
	}

	// The failing path must shut down the sibling proxy listener so its port is
	// released; otherwise RunContext returns while the listener is still bound.
	freed, derr := net.Listen("tcp", proxyAddr)
	if derr != nil {
		t.Fatalf("proxy port %s not released after metrics server failure: %v", proxyAddr, derr)
	}
	_ = freed.Close()
}
