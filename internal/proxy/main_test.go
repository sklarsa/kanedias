package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type receivedRequest struct {
	host          string
	authorization string
	apiKey        string
	accountID     string
}

func TestProxyInjectsProviderCredentials(t *testing.T) {
	ca, caPEM, _, err := generateCA("test proxy")
	if err != nil {
		t.Fatal(err)
	}

	received := make(chan receivedRequest, 1)
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- receivedRequest{
			host:          r.Host,
			authorization: r.Header.Get("Authorization"),
			apiKey:        r.Header.Get("X-Api-Key"),
			accountID:     r.Header.Get("Chatgpt-Account-Id"),
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()

	handler := newProxy(ca, credentials{
		github:    "github-secret",
		anthropic: "anthropic-secret",
		openAI:    "openai-secret",
	})
	handler.Tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, upstream.Listener.Addr().String())
	}
	handler.Tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // local fake upstream

	proxyServer := httptest.NewServer(handler)
	defer proxyServer.Close()
	proxyURL, err := url.Parse(proxyServer.URL)
	if err != nil {
		t.Fatal(err)
	}

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		t.Fatal("failed to trust test proxy CA")
	}
	client := &http.Client{Transport: &http.Transport{
		Proxy: http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			RootCAs:    roots,
		},
	}}

	tests := []struct {
		name              string
		host              string
		wantAuthorization string
		wantAPIKey        string
	}{
		{name: "GitHub API", host: "api.github.com", wantAuthorization: "Bearer github-secret"},
		{name: "GitHub uploads", host: "uploads.github.com", wantAuthorization: "Bearer github-secret"},
		{name: "GitHub Git", host: "github.com", wantAuthorization: "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:github-secret"))},
		{name: "Anthropic", host: "api.anthropic.com", wantAPIKey: "anthropic-secret"},
		{name: "OpenAI", host: "api.openai.com", wantAuthorization: "Bearer openai-secret"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, "https://"+tc.host+"/test", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer container-dummy")
			req.Header.Set("X-Api-Key", "container-dummy")

			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}

			got := <-received
			if got.host != tc.host {
				t.Errorf("host = %q, want %q", got.host, tc.host)
			}
			if got.authorization != tc.wantAuthorization {
				t.Errorf("Authorization = %q, want %q", got.authorization, tc.wantAuthorization)
			}
			if got.apiKey != tc.wantAPIKey {
				t.Errorf("X-Api-Key = %q, want %q", got.apiKey, tc.wantAPIKey)
			}
		})
	}
}

func TestProxyInjectsOAuthCredentials(t *testing.T) {
	ca, caPEM, _, err := generateCA("test proxy")
	if err != nil {
		t.Fatal(err)
	}

	received := make(chan receivedRequest, 1)
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- receivedRequest{
			host:          r.Host,
			authorization: r.Header.Get("Authorization"),
			apiKey:        r.Header.Get("X-Api-Key"),
			accountID:     r.Header.Get("Chatgpt-Account-Id"),
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()

	handler := newProxy(ca, credentials{
		anthropicOAuth: bearerTokenSourceFunc(func(context.Context) (bearerToken, error) {
			return bearerToken{access: "anthropic-oauth"}, nil
		}),
		openAICodexOAuth: bearerTokenSourceFunc(func(context.Context) (bearerToken, error) {
			return bearerToken{access: "codex-oauth", accountID: "account-123"}, nil
		}),
	})
	handler.Tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, upstream.Listener.Addr().String())
	}
	handler.Tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // local fake upstream

	proxyServer := httptest.NewServer(handler)
	defer proxyServer.Close()
	proxyURL, err := url.Parse(proxyServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		t.Fatal("failed to trust test proxy CA")
	}
	client := &http.Client{Transport: &http.Transport{
		Proxy:           http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
	}}

	tests := []struct {
		host          string
		authorization string
		accountID     string
	}{
		{host: "api.anthropic.com", authorization: "Bearer anthropic-oauth"},
		{host: "chatgpt.com", authorization: "Bearer codex-oauth", accountID: "account-123"},
	}
	for _, tc := range tests {
		t.Run(tc.host, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, "https://"+tc.host+"/test", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer container-dummy")
			req.Header.Set("X-Api-Key", "container-dummy")
			req.Header.Set("Chatgpt-Account-Id", "container-account")
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			got := <-received
			if got.authorization != tc.authorization {
				t.Errorf("Authorization = %q, want %q", got.authorization, tc.authorization)
			}
			if got.apiKey != "" {
				t.Errorf("X-Api-Key = %q, want empty", got.apiKey)
			}
			if got.accountID != tc.accountID {
				t.Errorf("Chatgpt-Account-Id = %q, want %q", got.accountID, tc.accountID)
			}
		})
	}
}

func TestGitHubCLIWorksThroughProxy(t *testing.T) {
	gh, err := exec.LookPath("gh")
	if err != nil {
		t.Skip("gh is not installed")
	}
	ca, caPEM, _, err := generateCA("test proxy")
	if err != nil {
		t.Fatal(err)
	}

	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer github-secret" {
			t.Errorf("Authorization = %q, want injected GitHub credential", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"login":"proxy-test"}`)
	}))
	defer upstream.Close()

	handler := newProxy(ca, credentials{github: "github-secret"})
	handler.Tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, upstream.Listener.Addr().String())
	}
	handler.Tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // local fake upstream
	proxyServer := httptest.NewServer(handler)
	defer proxyServer.Close()

	caPath := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(caPath, caPEM, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(gh, "api", "user")
	cmd.Env = cleanProxyEnv(os.Environ())
	cmd.Env = append(cmd.Env,
		"GH_TOKEN=container-dummy",
		"GH_CONFIG_DIR="+t.TempDir(),
		"HTTPS_PROXY="+proxyServer.URL,
		"SSL_CERT_FILE="+caPath,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gh api user failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), `"login":"proxy-test"`) {
		t.Fatalf("unexpected gh output: %s", output)
	}
}

func cleanProxyEnv(env []string) []string {
	cleaned := env[:0]
	for _, value := range env {
		key, _, _ := strings.Cut(value, "=")
		switch strings.ToLower(key) {
		case "gh_token", "github_token", "https_proxy", "no_proxy", "ssl_cert_file", "gh_config_dir":
			continue
		}
		cleaned = append(cleaned, value)
	}
	return cleaned
}

func TestProxyRejectsMismatchedProviderAuthority(t *testing.T) {
	ca, caPEM, _, err := generateCA("test proxy")
	if err != nil {
		t.Fatal(err)
	}

	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("mismatched authority reached upstream with Host %q", r.Host)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	handler := newProxy(ca, credentials{github: "github-secret"})
	handler.Tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, upstream.Listener.Addr().String())
	}
	handler.Tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // local fake upstream
	proxyServer := httptest.NewServer(handler)
	defer proxyServer.Close()
	proxyURL, err := url.Parse(proxyServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		t.Fatal("failed to trust test proxy CA")
	}
	client := &http.Client{Transport: &http.Transport{
		Proxy:           http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
	}}
	req, err := http.NewRequest(http.MethodGet, "https://api.github.com/test", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "evil.example"
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("status = %d, want 421", resp.StatusCode)
	}
}

func TestProxyRejectsPlaintextRequestsForCredentialedHosts(t *testing.T) {
	ca, _, _, err := generateCA("test proxy")
	if err != nil {
		t.Fatal(err)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("plaintext request reached upstream with Authorization %q", r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	handler := newProxy(ca, credentials{github: "github-secret"})
	handler.Tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, upstream.Listener.Addr().String())
	}
	proxyServer := httptest.NewServer(handler)
	defer proxyServer.Close()
	proxyURL, err := url.Parse(proxyServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}

	resp, err := client.Get("http://api.github.com/test")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestProxyTunnelsOtherHostsWithoutChangingRequests(t *testing.T) {
	ca, _, _, err := generateCA("test proxy")
	if err != nil {
		t.Fatal(err)
	}

	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer original" {
			t.Errorf("Authorization = %q, want unchanged header", got)
		}
		_, _ = io.WriteString(w, "tunneled")
	}))
	defer upstream.Close()

	handler := newProxy(ca, credentials{})
	proxyServer := httptest.NewServer(handler)
	defer proxyServer.Close()
	proxyURL, err := url.Parse(proxyServer.URL)
	if err != nil {
		t.Fatal(err)
	}

	transport := upstream.Client().Transport.(*http.Transport).Clone()
	transport.Proxy = http.ProxyURL(proxyURL)
	client := &http.Client{Transport: transport}
	req, err := http.NewRequest(http.MethodGet, upstream.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer original")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "tunneled" {
		t.Fatalf("body = %q, want tunneled", body)
	}
}

func TestProxyPassesAnonymousGitHubWebTraffic(t *testing.T) {
	ca, caPEM, _, err := generateCA("anonymous GitHub test")
	if err != nil {
		t.Fatal(err)
	}

	receivedAuthorization := make(chan string, 1)
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuthorization <- r.Header.Get("Authorization")
		_, _ = io.WriteString(w, "github home")
	}))
	defer upstream.Close()

	handler := newProxy(ca, credentials{})
	handler.Tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, upstream.Listener.Addr().String())
	}
	handler.Tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // local fake upstream
	proxyServer := httptest.NewServer(handler)
	defer proxyServer.Close()

	resp, err := observedMITMClient(t, proxyServer.URL, caPEM).Get("https://github.com/")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %q", resp.StatusCode, body)
	}
	if string(body) != "github home" {
		t.Fatalf("body = %q, want %q", body, "github home")
	}
	if authorization := <-receivedAuthorization; authorization != "" {
		t.Fatalf("Authorization = %q, want empty", authorization)
	}
}

func TestProxyReturnsAgentFriendlyGitHubAuthError(t *testing.T) {
	ca, caPEM, _, err := generateCA("missing GitHub auth test")
	if err != nil {
		t.Fatal(err)
	}
	handler := newProxy(ca, credentials{})
	proxyServer := httptest.NewServer(handler)
	defer proxyServer.Close()
	client := observedMITMClient(t, proxyServer.URL, caPEM)

	tests := []struct {
		name          string
		url           string
		authorization string
	}{
		{name: "API", url: "https://api.github.com/user"},
		{name: "authenticated web", url: "https://github.com/owner/repository.git", authorization: "Basic sandbox-placeholder"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, test.url, nil)
			if err != nil {
				t.Fatal(err)
			}
			if test.authorization != "" {
				req.Header.Set("Authorization", test.authorization)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusBadGateway {
				t.Errorf("status = %d, want 502", resp.StatusCode)
			}
			if contentType := resp.Header.Get("Content-Type"); contentType != "text/plain" {
				t.Errorf("Content-Type = %q, want text/plain", contentType)
			}
			if string(body) != "GitHub auth unavailable" {
				t.Errorf("body = %q, want %q", body, "GitHub auth unavailable")
			}
		})
	}
}

func TestInitCAGeneratesValidKeyPair(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.crt")
	keyPath := filepath.Join(dir, "ca.key")

	if err := InitCA(certPath, keyPath); err != nil {
		t.Fatalf("initialize CA: %v", err)
	}

	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		t.Fatalf("generated CA is not a valid key pair: %v", err)
	}
}

func TestLoadOrCreateCA(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.crt")
	keyPath := filepath.Join(dir, "ca.key")

	first, certPEM, err := loadOrCreateCA(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	second, secondPEM, err := loadOrCreateCA(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Certificate) == 0 || len(second.Certificate) == 0 {
		t.Fatal("loaded CA has no certificate")
	}
	if string(certPEM) != string(secondPEM) {
		t.Fatal("second load generated a different CA")
	}
}
