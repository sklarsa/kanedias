package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestPrivacySafeProxyLoggerClassifiesWithoutRenderingArguments(t *testing.T) {
	tests := []struct {
		name   string
		format string
		want   string
	}{
		{name: "connect", format: "[%03d] WARN: Error dialing to %s: %s", want: "connect_dial"},
		{name: "TLS", format: "[%03d] WARN: Cannot handshake client %v %v", want: "tls_handshake"},
		{name: "client read", format: "[%03d] WARN: Cannot read request from mitm'd client %v %v", want: "client_read"},
		{name: "upstream read", format: "[%03d] WARN: Cannot read response from mitm'd server %v", want: "upstream_read"},
		{name: "client write", format: "[%03d] WARN: Cannot write response from mitm'd client: %v", want: "client_write"},
		{name: "copy", format: "[%03d] WARN: Error copying to client: %s", want: "tunnel_copy"},
		{name: "websocket", format: "[%03d] WARN: Unable to use Websocket connection", want: "websocket"},
		{name: "certificate", format: "[%03d] WARN: Cannot sign host certificate with provided CA: %s", want: "certificate"},
		{name: "protocol", format: "[%03d] WARN: HTTP2 connection failed: %v", want: "protocol"},
		{name: "unknown", format: "unknown %s", want: "internal"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var logs bytes.Buffer
			logger := privacySafeProxyLogger{logger: slog.New(slog.NewJSONHandler(&logs, nil))}
			logger.Printf(test.format, 7, "private.example", errors.New("secret-error-token"))
			output := logs.String()
			if !strings.Contains(output, `"error_class":"`+test.want+`"`) {
				t.Fatalf("classified log = %s, want %s", output, test.want)
			}
			for _, secret := range []string{"private.example", "secret-error-token"} {
				if strings.Contains(output, secret) {
					t.Fatalf("secret %q leaked in %s", secret, output)
				}
			}
		})
	}
}

func TestPrivacySafeProxyLoggerSuppressesExpectedTeardown(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "context canceled", err: context.Canceled},
		{name: "EOF", err: io.EOF},
		{name: "unexpected EOF", err: io.ErrUnexpectedEOF},
		{name: "closed connection", err: net.ErrClosed},
		{name: "broken pipe", err: syscall.EPIPE},
		{name: "connection reset", err: syscall.ECONNRESET},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var logs bytes.Buffer
			logger := privacySafeProxyLogger{logger: slog.New(slog.NewJSONHandler(&logs, nil))}
			logger.Printf("proxy warning: %v", fmt.Errorf("wrapped teardown: %w", test.err))
			if logs.Len() != 0 {
				t.Fatalf("expected teardown produced log: %s", logs.String())
			}
		})
	}
}

func TestProxyRouteForHostIsBounded(t *testing.T) {
	tests := []struct {
		host string
		want string
	}{
		{"api.github.com", "github"},
		{"uploads.github.com", "github"},
		{"github.com", "github"},
		{"API.ANTHROPIC.COM", "anthropic"},
		{"api.openai.com", "openai"},
		{"chatgpt.com", "openai_codex"},
		{"example.com", "passthrough"},
		{"", "passthrough"},
	}
	for _, test := range tests {
		t.Run(test.host, func(t *testing.T) {
			if got := proxyRouteForHost(test.host); got != test.want {
				t.Fatalf("proxyRouteForHost(%q) = %q, want %q", test.host, got, test.want)
			}
		})
	}
}

func TestProxyMethodIsBounded(t *testing.T) {
	tests := []struct {
		method string
		want   string
	}{
		{"GET", "GET"},
		{"post", "POST"},
		{"PATCH", "PATCH"},
		{"CONNECT", "CONNECT"},
		{"BREW", "OTHER"},
		{"", "OTHER"},
	}
	for _, test := range tests {
		if got := proxyMethod(test.method); got != test.want {
			t.Errorf("proxyMethod(%q) = %q, want %q", test.method, got, test.want)
		}
	}
}

func TestProxyStatusClassIsBounded(t *testing.T) {
	tests := []struct {
		status int
		want   string
	}{
		{0, "none"},
		{199, "1xx"},
		{200, "2xx"},
		{302, "3xx"},
		{404, "4xx"},
		{503, "5xx"},
		{700, "other"},
	}
	for _, test := range tests {
		if got := proxyStatusClass(test.status); got != test.want {
			t.Errorf("proxyStatusClass(%d) = %q, want %q", test.status, got, test.want)
		}
	}
}

func TestProxyMetricsHandlerExposesOnlyMetrics(t *testing.T) {
	metrics, registry := newProxyMetrics()
	metrics.connectDecision("github", "mitm")
	handler := newMetricsHandler(registry)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("metrics status = %d, want 200", resp.Code)
	}
	body := resp.Body.String()
	for _, name := range []string{"kanedias_proxy_connect_total", "go_goroutines", "process_cpu_seconds_total"} {
		if !strings.Contains(body, name) {
			t.Errorf("metrics output missing %q", name)
		}
	}

	req = httptest.NewRequest(http.MethodGet, "/", nil)
	resp = httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusNotFound {
		t.Fatalf("root status = %d, want 404", resp.Code)
	}
}

func TestProxyMetricsRecordBoundedSignals(t *testing.T) {
	metrics, registry := newProxyMetrics()

	metrics.requestStarted("anthropic")
	if got := testutil.ToFloat64(metrics.inFlight.WithLabelValues("anthropic")); got != 1 {
		t.Fatalf("in-flight after start = %v, want 1", got)
	}
	metrics.requestFinished("anthropic", "POST", 200, "response", 125*time.Millisecond)
	metrics.connectDecision("passthrough", "tunnel")
	metrics.upstreamError("openai")
	metrics.credentialResult("github", "injected")
	metrics.credentialResult("openai", "missing")

	if got := testutil.ToFloat64(metrics.inFlight.WithLabelValues("anthropic")); got != 0 {
		t.Fatalf("in-flight after finish = %v, want 0", got)
	}
	if got := testutil.ToFloat64(metrics.requests.WithLabelValues("anthropic", "POST", "2xx", "response")); got != 1 {
		t.Fatalf("request count = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.connects.WithLabelValues("passthrough", "tunnel")); got != 1 {
		t.Fatalf("connect count = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.upstreamErrors.WithLabelValues("openai")); got != 1 {
		t.Fatalf("upstream error count = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.credentials.WithLabelValues("github", "injected")); got != 1 {
		t.Fatalf("credential injection count = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.credentials.WithLabelValues("openai", "missing")); got != 1 {
		t.Fatalf("missing credential count = %v, want 1", got)
	}

	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	foundDuration := false
	for _, family := range families {
		if family.GetName() == "kanedias_proxy_request_duration_seconds" {
			foundDuration = true
			if len(family.Metric) != 1 || family.Metric[0].GetHistogram().GetSampleCount() != 1 {
				t.Fatalf("duration histogram did not record one request")
			}
		}
	}
	if !foundDuration {
		t.Fatal("duration histogram was not registered")
	}
}
