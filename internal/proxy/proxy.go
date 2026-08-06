package proxy

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/elazarl/goproxy"
)

type credentials struct {
	github           string
	anthropic        string
	openAI           string
	anthropicOAuth   bearerTokenSource
	openAICodexOAuth bearerTokenSource
}

var interceptedHosts = map[string]struct{}{
	"api.github.com":     {},
	"uploads.github.com": {},
	"github.com":         {},
	"api.anthropic.com":  {},
	"api.openai.com":     {},
	"chatgpt.com":        {},
}

type oauthPaths struct {
	claude      string
	openAICodex string
}

func defaultOAuthPaths(configDir, homeDir string) oauthPaths {
	return oauthPaths{
		claude:      filepath.Join(homeDir, ".claude", ".credentials.json"),
		openAICodex: filepath.Join(configDir, "kanedias-proxy", "openai-codex.json"),
	}
}

func loadCredentials(claudeCredentialsPath, openAICodexAuthPath string) credentials {
	github := strings.TrimSpace(os.Getenv("GH_TOKEN"))
	if github == "" {
		output, err := exec.Command("gh", "auth", "token", "--hostname", "github.com").Output()
		if err == nil {
			github = strings.TrimSpace(string(output))
		}
	}

	var anthropicOAuth bearerTokenSource = newClaudeOAuthSource(claudeCredentialsPath)
	for _, name := range []string{"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_OAUTH_TOKEN", "ANTHROPIC_AUTH_TOKEN"} {
		if token := strings.TrimSpace(os.Getenv(name)); token != "" {
			anthropicOAuth = bearerTokenSourceFunc(func(context.Context) (bearerToken, error) {
				return bearerToken{access: token}, nil
			})
			break
		}
	}

	return credentials{
		github:           github,
		anthropic:        strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")),
		openAI:           strings.TrimSpace(os.Getenv("OPENAI_API_KEY")),
		anthropicOAuth:   anthropicOAuth,
		openAICodexOAuth: newOpenAICodexOAuthSource(openAICodexAuthPath),
	}
}

func newProxy(ca tls.Certificate, creds credentials) *goproxy.ProxyHttpServer {
	return newProxyWithObserver(ca, creds, nil)
}

func newProxyWithObserver(ca tls.Certificate, creds credentials, observer *proxyObserver) *goproxy.ProxyHttpServer {
	proxy := goproxy.NewProxyHttpServer()
	internalLogger := slog.Default()
	if observer != nil {
		internalLogger = observer.logger
	}
	proxy.Logger = privacySafeProxyLogger{logger: internalLogger}
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	proxy.ConnectDial = dialer.Dial
	proxy.Tr = &http.Transport{
		DialContext:         dialer.DialContext,
		ForceAttemptHTTP2:   true,
		IdleConnTimeout:     90 * time.Second,
		MaxIdleConns:        100,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout: 10 * time.Second,
	}
	if observer != nil {
		proxy.OnRequest().DoFunc(func(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
			observer.requestStarted(req, ctx, proxy.Tr)
			return req, nil
		})
		proxy.OnResponse().DoFunc(func(resp *http.Response, ctx *goproxy.ProxyCtx) *http.Response {
			observer.requestFinished(ctx, resp, nil)
			return resp
		})
	}

	mitm := &goproxy.ConnectAction{
		Action:    goproxy.ConnectMitm,
		TLSConfig: goproxy.TLSConfigFromCA(&ca),
	}
	accept := &goproxy.ConnectAction{Action: goproxy.ConnectAccept}
	proxy.OnRequest().HandleConnectFunc(func(address string, ctx *goproxy.ProxyCtx) (*goproxy.ConnectAction, string) {
		host := address
		if parsedHost, _, err := net.SplitHostPort(address); err == nil {
			host = parsedHost
		}
		route := proxyRouteForHost(host)
		if _, ok := interceptedHosts[strings.ToLower(host)]; ok {
			observer.connectDecision(ctx, address, route, "mitm")
			return mitm, address
		}
		observer.connectDecision(ctx, address, route, "tunnel")
		return accept, address
	})

	proxy.OnRequest().DoFunc(func(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
		host := strings.ToLower(req.URL.Hostname())
		if _, ok := interceptedHosts[host]; !ok {
			return req, nil
		}
		if req.URL.Scheme != "https" {
			return req, goproxy.NewResponse(req, goproxy.ContentTypeText,
				http.StatusBadRequest, "credentials require HTTPS")
		}
		if !requestAuthorityMatchesURL(req) {
			return req, goproxy.NewResponse(req, goproxy.ContentTypeText,
				http.StatusMisdirectedRequest, "request authority does not match CONNECT target")
		}

		if host == "github.com" && req.Header.Get("Authorization") == "" {
			return req, nil
		}

		req.Header.Del("Authorization")
		req.Header.Del("X-Api-Key")
		req.Header.Del("Chatgpt-Account-Id")
		switch host {
		case "api.github.com", "uploads.github.com":
			if creds.github == "" {
				observer.credentialResult("github", "missing")
				return req, missingGitHubCredential(req)
			}
			observer.credentialResult("github", "injected")
			req.Header.Set("Authorization", "Bearer "+creds.github)
		case "github.com":
			if creds.github == "" {
				observer.credentialResult("github", "missing")
				return req, missingGitHubCredential(req)
			}
			observer.credentialResult("github", "injected")
			req.SetBasicAuth("x-access-token", creds.github)
		case "api.anthropic.com":
			if creds.anthropic != "" {
				observer.credentialResult("anthropic", "injected")
				req.Header.Set("X-Api-Key", creds.anthropic)
				break
			}
			token, err := resolveBearerToken(req, ctx, "Anthropic", creds.anthropicOAuth)
			if err != nil {
				observer.credentialResult("anthropic", "missing")
				return req, missingCredential(req, "Anthropic")
			}
			observer.credentialResult("anthropic", "injected")
			req.Header.Set("Authorization", "Bearer "+token.access)
		case "api.openai.com":
			if creds.openAI == "" {
				observer.credentialResult("openai", "missing")
				return req, missingCredential(req, "OpenAI")
			}
			observer.credentialResult("openai", "injected")
			req.Header.Set("Authorization", "Bearer "+creds.openAI)
		case "chatgpt.com":
			token, err := resolveBearerToken(req, ctx, "OpenAI Codex", creds.openAICodexOAuth)
			if err != nil || token.accountID == "" {
				observer.credentialResult("openai_codex", "missing")
				return req, missingCredential(req, "OpenAI Codex")
			}
			observer.credentialResult("openai_codex", "injected")
			req.Header.Set("Authorization", "Bearer "+token.access)
			req.Header.Set("Chatgpt-Account-Id", token.accountID)
		}
		return req, nil
	})
	return proxy
}

func requestAuthorityMatchesURL(req *http.Request) bool {
	expected, err := canonicalAuthority(req.URL.Host, req.URL.Scheme)
	if err != nil {
		return false
	}
	actual, err := canonicalAuthority(req.Host, req.URL.Scheme)
	return err == nil && actual == expected
}

func canonicalAuthority(authority, scheme string) (string, error) {
	host, port, err := net.SplitHostPort(authority)
	if err != nil {
		if strings.Contains(authority, ":") {
			return "", err
		}
		host = authority
	}
	if host == "" {
		return "", errors.New("empty host")
	}
	if port == "" {
		switch scheme {
		case "https":
			port = "443"
		case "http":
			port = "80"
		default:
			return "", fmt.Errorf("unsupported URL scheme %q", scheme)
		}
	}
	return strings.ToLower(host) + ":" + port, nil
}

func resolveBearerToken(req *http.Request, ctx *goproxy.ProxyCtx, provider string, source bearerTokenSource) (bearerToken, error) {
	if source == nil {
		return bearerToken{}, errors.New("credential source is not configured")
	}
	token, err := source.Token(req.Context())
	if err != nil {
		ctx.Warnf("%s credential unavailable", provider)
		return bearerToken{}, err
	}
	if token.access == "" {
		return bearerToken{}, errors.New("credential source returned an empty token")
	}
	return token, nil
}

func missingGitHubCredential(req *http.Request) *http.Response {
	return goproxy.NewResponse(req, goproxy.ContentTypeText, http.StatusBadGateway,
		"GitHub auth unavailable")
}

func missingCredential(req *http.Request, provider string) *http.Response {
	return goproxy.NewResponse(req, goproxy.ContentTypeText, http.StatusBadGateway,
		provider+" credential is not configured")
}

func loadOrCreateCA(certPath, keyPath string) (tls.Certificate, []byte, error) {
	certPEM, certErr := os.ReadFile(certPath)
	keyPEM, keyErr := os.ReadFile(keyPath)
	if certErr == nil && keyErr == nil {
		ca, err := tls.X509KeyPair(certPEM, keyPEM)
		return ca, certPEM, err
	}
	if !errors.Is(certErr, os.ErrNotExist) || !errors.Is(keyErr, os.ErrNotExist) {
		return tls.Certificate{}, nil, fmt.Errorf("CA certificate and key must both exist or both be absent")
	}

	ca, certPEM, keyPEM, err := generateCA("kanedias proxy")
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	if err := os.MkdirAll(filepath.Dir(certPath), 0700); err != nil {
		return tls.Certificate{}, nil, err
	}
	if filepath.Dir(keyPath) != filepath.Dir(certPath) {
		if err := os.MkdirAll(filepath.Dir(keyPath), 0700); err != nil {
			return tls.Certificate{}, nil, err
		}
	}
	if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
		return tls.Certificate{}, nil, err
	}
	if err := os.Chmod(keyPath, 0600); err != nil {
		return tls.Certificate{}, nil, err
	}
	if err := os.WriteFile(certPath, certPEM, 0644); err != nil {
		return tls.Certificate{}, nil, err
	}
	if err := os.Chmod(certPath, 0644); err != nil {
		return tls.Certificate{}, nil, err
	}
	return ca, certPEM, nil
}

func generateCA(commonName string) (tls.Certificate, []byte, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, nil, err
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return tls.Certificate{}, nil, nil, err
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return tls.Certificate{}, nil, nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	ca, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, nil, nil, err
	}
	ca.Leaf, err = x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, nil, nil, err
	}
	return ca, certPEM, keyPEM, nil
}
