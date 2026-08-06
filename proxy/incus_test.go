//go:build incus

package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestProxyPassesUnauthenticatedHTTPSFromIncus(t *testing.T) {
	if _, err := exec.LookPath("incus"); err != nil {
		t.Fatal("incus is required")
	}

	const marker = "through-kanedias-proxy"
	target := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("Proxy-Authorization") != "" {
			http.Error(w, "unexpected authentication", http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, marker)
	}))
	_ = target.Listener.Close()
	listener, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	target.Listener = listener
	target.StartTLS()
	t.Cleanup(target.Close)
	targetPort := listener.Addr().(*net.TCPAddr).Port

	ca, _, _, err := generateCA("Incus proxy test")
	if err != nil {
		t.Fatal(err)
	}
	proxy := newProxy(ca, credentials{})
	proxy.Logger = log.New(io.Discard, "", 0)
	connected := make(chan string, 1)
	dial := proxy.ConnectDial
	proxy.ConnectDial = func(network, address string) (net.Conn, error) {
		select {
		case connected <- address:
		default:
		}
		return dial(network, address)
	}
	proxyListener, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	proxyPort := proxyListener.Addr().(*net.TCPAddr).Port
	proxyServer := &http.Server{Handler: proxy, ReadHeaderTimeout: 10 * time.Second}
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- proxyServer.Serve(proxyListener)
	}()
	t.Cleanup(func() {
		_ = proxyServer.Close()
		if err := <-serverDone; err != nil && err != http.ErrServerClosed {
			t.Errorf("proxy server: %v", err)
		}
	})

	image := os.Getenv("INCUS_TEST_IMAGE")
	if image == "" {
		image = "images:alpine/3.22"
	}
	container := fmt.Sprintf("kanedias-proxy-e2e-%d-%d", os.Getpid(), time.Now().UnixNano())
	if output, err := incusCommand("launch", image, container); err != nil {
		t.Fatalf("launch Incus container: %v\n%s", err, output)
	}
	t.Cleanup(func() {
		if output, err := incusCommand("delete", "--force", container); err != nil {
			t.Errorf("delete Incus container: %v\n%s", err, output)
		}
	})

	gateway := waitForIncusGateway(t, container)
	if output, err := incusCommand(
		"exec", container, "--", "sh", "-c",
		"command -v curl >/dev/null || apk add --no-cache curl",
	); err != nil {
		t.Fatalf("install curl in container: %v\n%s", err, output)
	}

	proxyURL := "http://" + net.JoinHostPort(gateway, strconv.Itoa(proxyPort))
	targetAddress := net.JoinHostPort(gateway, strconv.Itoa(targetPort))
	targetURL := "https://" + targetAddress + "/"
	output, err := incusCommand(
		"exec", container, "--",
		"curl", "--silent", "--show-error", "--fail", "--insecure",
		"--noproxy", "", "--proxy", proxyURL, targetURL,
	)
	if err != nil {
		t.Fatalf("request through proxy: %v\n%s", err, output)
	}
	if strings.TrimSpace(output) != marker {
		t.Fatalf("response = %q, want %q", strings.TrimSpace(output), marker)
	}
	select {
	case address := <-connected:
		if address != targetAddress {
			t.Fatalf("proxy connected to %q, want %q", address, targetAddress)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("request reached target without a CONNECT through the proxy")
	}
}

func waitForIncusGateway(t *testing.T, container string) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		output, err := incusCommand(
			"exec", container, "--", "sh", "-c",
			"ip route | awk '$1 == \"default\" { print $3; exit }'",
		)
		gateway := strings.TrimSpace(output)
		if err == nil && net.ParseIP(gateway) != nil {
			return gateway
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("container %s did not acquire a default gateway", container)
	return ""
}

func incusCommand(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	output, err := exec.CommandContext(ctx, "incus", args...).CombinedOutput()
	return string(output), err
}
