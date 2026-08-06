//go:build incus

package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestLiveClientsThroughProxy(t *testing.T) {
	if os.Getenv("KANEDIAS_LIVE_SMOKE") != "1" {
		t.Skip("set KANEDIAS_LIVE_SMOKE=1 to run paid live provider smoke tests")
	}

	configDir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	paths := defaultOAuthPaths(configDir, homeDir)
	if _, err := os.Stat(paths.openAICodex); err != nil {
		t.Fatalf("OpenAI Codex login is required; run go run ./proxy -login-openai-codex: %v", err)
	}

	ca, caPEM, _, err := generateCA("kanedias live smoke")
	if err != nil {
		t.Fatal(err)
	}
	proxy := newProxy(ca, loadCredentials(paths.claude, paths.openAICodex))
	proxy.Logger = log.New(io.Discard, "", 0)
	listener, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	proxyPort := listener.Addr().(*net.TCPAddr).Port
	server := &http.Server{Handler: proxy, ReadHeaderTimeout: 10 * time.Second}
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Close()
		if err := <-serverDone; err != nil && err != http.ErrServerClosed {
			t.Errorf("proxy server: %v", err)
		}
	})

	image := os.Getenv("INCUS_SMOKE_IMAGE")
	if image == "" {
		image = "sandbox"
	}
	container := fmt.Sprintf("kanedias-live-smoke-%d-%d", os.Getpid(), time.Now().UnixNano())
	if output, err := incusCommand("launch", image, container); err != nil {
		t.Fatalf("launch Incus container: %v\n%s", err, output)
	}
	t.Cleanup(func() {
		if output, err := incusCommand("delete", "--force", container); err != nil {
			t.Errorf("delete Incus container: %v\n%s", err, output)
		}
	})

	gateway := waitForIncusGateway(t, container)
	proxyURL := "http://" + net.JoinHostPort(gateway, strconv.Itoa(proxyPort))
	caPath := filepath.Join(t.TempDir(), "kanedias-proxy.crt")
	if err := os.WriteFile(caPath, caPEM, 0600); err != nil {
		t.Fatal(err)
	}
	if output, err := incusCommand("file", "push", caPath, container+"/usr/local/share/ca-certificates/kanedias-proxy.crt"); err != nil {
		t.Fatalf("push proxy CA: %v\n%s", err, output)
	}
	if output, err := incusCommand("exec", container, "--", "update-ca-certificates"); err != nil {
		t.Fatalf("trust proxy CA: %v\n%s", err, output)
	}
	if output, err := incusCommand("exec", container, "--", "install", "-d", "-o", "kanedias", "-g", "kanedias", "/tmp/kanedias-smoke"); err != nil {
		t.Fatalf("create isolated smoke-test home: %v\n%s", err, output)
	}

	baseEnv := []string{
		"HOME=/tmp/kanedias-smoke",
		"USER=kanedias",
		"LOGNAME=kanedias",
		"HTTPS_PROXY=" + proxyURL,
		"https_proxy=" + proxyURL,
		"HTTP_PROXY=" + proxyURL,
		"http_proxy=" + proxyURL,
		"NO_PROXY=",
		"no_proxy=",
		"NODE_EXTRA_CA_CERTS=/usr/local/share/ca-certificates/kanedias-proxy.crt",
		"SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt",
	}

	t.Run("gh", func(t *testing.T) {
		output := runLiveClient(t, container, append(baseEnv, "GH_TOKEN=container-dummy"),
			"gh api user --jq .login")
		if strings.TrimSpace(output) == "" {
			t.Fatal("gh returned an empty login")
		}
	})

	t.Run("claude", func(t *testing.T) {
		output := runLiveClient(t, container, append(baseEnv,
			"CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-container-dummy",
			"DISABLE_AUTOUPDATER=1",
			"DISABLE_TELEMETRY=1",
		), "claude -p 'Reply with exactly kanedias-claude-ok' --model sonnet --output-format text --max-turns 1 --tools ''")
		if !strings.Contains(strings.ToLower(output), "kanedias-claude-ok") {
			t.Fatalf("unexpected Claude output: %s", output)
		}
	})

	t.Run("pi", func(t *testing.T) {
		dummyJWT := fakeCodexJWT(t, "container-account")
		script := "set -e; mkdir -p \"$HOME/.pi/agent\"; " +
			"printf '%s\\n' '{\"transport\":\"sse\",\"defaultProjectTrust\":\"never\"}' > \"$HOME/.pi/agent/settings.json\"; " +
			"printf '{\"openai-codex\":{\"type\":\"oauth\",\"access\":\"%s\",\"refresh\":\"container-dummy\",\"expires\":9999999999999,\"accountId\":\"container-account\"}}\\n' \"$CODEX_DUMMY_TOKEN\" > \"$HOME/.pi/agent/auth.json\"; " +
			"chmod 600 \"$HOME/.pi/agent/auth.json\"; " +
			"export NVM_DIR=/home/kanedias/.nvm; . \"$NVM_DIR/nvm.sh\"; " +
			"pi --provider openai-codex --model gpt-5.6-sol --no-tools --no-session " +
			"-p 'Reply with exactly kanedias-pi-ok'"
		output := runLiveClient(t, container, append(baseEnv, "CODEX_DUMMY_TOKEN="+dummyJWT), script)
		if !strings.Contains(strings.ToLower(output), "kanedias-pi-ok") {
			t.Fatalf("unexpected Pi output: %s", output)
		}
	})
}

func runLiveClient(t *testing.T, container string, environment []string, script string) string {
	t.Helper()
	args := []string{"exec", container, "--", "runuser", "-u", "kanedias", "--", "env"}
	args = append(args, environment...)
	args = append(args, "bash", "-c", script)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	output, err := incusCommandContext(ctx, args...)
	if err != nil {
		t.Fatalf("live client failed: %v\n%s", err, output)
	}
	return output
}

func incusCommandContext(ctx context.Context, args ...string) (string, error) {
	output, err := exec.CommandContext(ctx, "incus", args...).CombinedOutput()
	return string(output), err
}
