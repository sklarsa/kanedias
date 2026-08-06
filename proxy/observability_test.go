package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

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
