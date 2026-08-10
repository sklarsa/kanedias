package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestObservedProxyLogsAndMeasuresInterceptedRequest(t *testing.T) {
	ca, caPEM, _, err := generateCA("observed proxy test")
	if err != nil {
		t.Fatal(err)
	}

	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()

	metrics, _ := newProxyMetrics()
	var logs bytes.Buffer
	observer := newProxyObserver(slog.New(slog.NewTextHandler(&logs, nil)), true, metrics)
	handler := newProxyWithObserver(ca, credentials{github: "host-github-secret"}, observer)
	handler.Tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, upstream.Listener.Addr().String())
	}
	handler.Tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // local fake upstream

	proxyServer := httptest.NewServer(handler)
	defer proxyServer.Close()
	client := observedMITMClient(t, proxyServer.URL, caPEM)

	req, err := http.NewRequest(http.MethodPost, "https://api.github.com/v1/private?token=query-secret", strings.NewReader("body-secret"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer container-secret")
	req.Header.Set("Cookie", "session=cookie-secret")
	req.Header.Set("X-Private", "header-secret")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	logOutput := logs.String()
	for _, fragment := range []string{`msg="proxy connect"`, `action=mitm`, `msg="proxy request"`, `route=github`, `method=POST`, `host=api.github.com`, `path=/v1/private`, `status=200`, `duration=`} {
		if !strings.Contains(logOutput, fragment) {
			t.Errorf("request log missing %s:\n%s", fragment, logOutput)
		}
	}
	for _, secret := range []string{"query-secret", "container-secret", "host-github-secret", "cookie-secret", "header-secret", "body-secret", "Authorization", "Cookie", "X-Private"} {
		if strings.Contains(logOutput, secret) {
			t.Errorf("request log leaked %q:\n%s", secret, logOutput)
		}
	}

	if got := testutil.ToFloat64(metrics.requests.WithLabelValues("github", "POST", "2xx", "response")); got != 1 {
		t.Fatalf("successful request count = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.inFlight.WithLabelValues("github")); got != 0 {
		t.Fatalf("in-flight requests = %v, want 0", got)
	}
	if got := testutil.ToFloat64(metrics.connects.WithLabelValues("github", "mitm")); got != 1 {
		t.Fatalf("MITM connect count = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.credentials.WithLabelValues("github", "injected")); got != 1 {
		t.Fatalf("credential injection count = %v, want 1", got)
	}
}

func TestObservedProxyMeasuresSyntheticMissingCredential(t *testing.T) {
	ca, caPEM, _, err := generateCA("observed missing credential test")
	if err != nil {
		t.Fatal(err)
	}
	metrics, _ := newProxyMetrics()
	var logs bytes.Buffer
	handler := newProxyWithObserver(ca, credentials{}, newProxyObserver(slog.New(slog.NewTextHandler(&logs, nil)), false, metrics))
	proxyServer := httptest.NewServer(handler)
	defer proxyServer.Close()

	resp, err := observedMITMClient(t, proxyServer.URL, caPEM).Get("https://api.github.com/user")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if got := testutil.ToFloat64(metrics.requests.WithLabelValues("github", "GET", "5xx", "response")); got != 1 {
		t.Fatalf("missing credential request count = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.credentials.WithLabelValues("github", "missing")); got != 1 {
		t.Fatalf("missing credential count = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.inFlight.WithLabelValues("github")); got != 0 {
		t.Fatalf("in-flight requests = %v, want 0", got)
	}
	if logs.Len() != 0 {
		t.Fatalf("request logging was disabled but produced: %s", logs.String())
	}
}

func TestObservedProxyFinishesUpstreamErrors(t *testing.T) {
	ca, _, _, err := generateCA("observed upstream error test")
	if err != nil {
		t.Fatal(err)
	}
	metrics, _ := newProxyMetrics()
	var logs bytes.Buffer
	handler := newProxyWithObserver(ca, credentials{}, newProxyObserver(slog.New(slog.NewJSONHandler(&logs, nil)), true, metrics))
	handler.Tr.DialContext = func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("upstream unavailable secret-error-token")
	}
	proxyServer := httptest.NewServer(handler)
	defer proxyServer.Close()
	proxyURL, err := url.Parse(proxyServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}

	resp, err := client.Get("http://example.test/private?secret=query-secret")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	if got := testutil.ToFloat64(metrics.requests.WithLabelValues("passthrough", "GET", "none", "upstream_error")); got != 1 {
		t.Fatalf("upstream error request count = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.upstreamErrors.WithLabelValues("passthrough")); got != 1 {
		t.Fatalf("upstream errors = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.inFlight.WithLabelValues("passthrough")); got != 0 {
		t.Fatalf("in-flight requests = %v, want 0", got)
	}
	for _, fragment := range []string{`"outcome":"upstream_error"`, `"error_class":"round_trip"`} {
		if !strings.Contains(logs.String(), fragment) {
			t.Fatalf("error log missing %s: %s", fragment, logs.String())
		}
	}
	for _, secret := range []string{"query-secret", "secret-error-token"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("secret %q leaked into error log: %s", secret, logs.String())
		}
	}
}

func TestObservedProxySanitizesMITMInternalErrors(t *testing.T) {
	ca, caPEM, _, err := generateCA("observed MITM error test")
	if err != nil {
		t.Fatal(err)
	}
	metrics, _ := newProxyMetrics()
	var logs bytes.Buffer
	observer := newProxyObserver(slog.New(slog.NewJSONHandler(&logs, nil)), true, metrics)
	handler := newProxyWithObserver(ca, credentials{github: "github-secret"}, observer)
	handler.Tr.DialContext = func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("mitm secret-error-token")
	}
	proxyServer := httptest.NewServer(handler)
	defer proxyServer.Close()

	_, err = observedMITMClient(t, proxyServer.URL, caPEM).Get("https://api.github.com/private")
	if err == nil {
		t.Fatal("MITM upstream failure unexpectedly succeeded")
	}
	logOutput := logs.String()
	if !strings.Contains(logOutput, `"msg":"proxy internal warning"`) || !strings.Contains(logOutput, `"error_class":"upstream_read"`) {
		t.Fatalf("sanitized goproxy warning missing: %s", logOutput)
	}
	if strings.Contains(logOutput, "secret-error-token") {
		t.Fatalf("MITM transport error leaked into logs: %s", logOutput)
	}
}

func TestObservedProxyMeasuresPassthroughTunnel(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "tunneled")
	}))
	defer upstream.Close()
	ca, _, _, err := generateCA("observed tunnel test")
	if err != nil {
		t.Fatal(err)
	}
	metrics, _ := newProxyMetrics()
	handler := newProxyWithObserver(ca, credentials{}, newProxyObserver(slog.New(slog.NewTextHandler(io.Discard, nil)), false, metrics))
	handler.ConnectDial = func(network, address string) (net.Conn, error) {
		return net.Dial(network, upstream.Listener.Addr().String())
	}
	proxyServer := httptest.NewServer(handler)
	defer proxyServer.Close()
	proxyURL, err := url.Parse(proxyServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	transport := upstream.Client().Transport.(*http.Transport).Clone()
	transport.Proxy = http.ProxyURL(proxyURL)
	client := &http.Client{Transport: transport}

	resp, err := client.Get(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if got := testutil.ToFloat64(metrics.connects.WithLabelValues("passthrough", "tunnel")); got != 1 {
		t.Fatalf("tunnel connect count = %v, want 1", got)
	}
}

func observedMITMClient(t *testing.T, proxyAddress string, caPEM []byte) *http.Client {
	t.Helper()
	proxyURL, err := url.Parse(proxyAddress)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		t.Fatal("failed to trust observed proxy CA")
	}
	return &http.Client{Transport: &http.Transport{
		Proxy:           http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
	}}
}
