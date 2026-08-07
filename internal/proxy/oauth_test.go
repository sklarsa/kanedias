package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestOAuthPaths(t *testing.T) {
	paths := defaultOAuthPaths("/home/test/.config", "/home/test")
	if paths.openAICodex != "/home/test/.config/kanedias-proxy/openai-codex.json" {
		t.Errorf("OpenAI path = %q", paths.openAICodex)
	}
	if paths.claude != "/home/test/.claude/.credentials.json" {
		t.Errorf("Claude path = %q", paths.claude)
	}
}

func TestClaudeOAuthRefreshesAndPersistsRotatedToken(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	path := filepath.Join(t.TempDir(), ".credentials.json")
	writeJSONFile(t, path, map[string]any{
		"claudeAiOauth": map[string]any{
			"accessToken":      "expired-access",
			"refreshToken":     "old-refresh",
			"expiresAt":        now.Add(-time.Minute).UnixMilli(),
			"scopes":           []string{"user:inference", "user:profile"},
			"subscriptionType": "max",
			"rateLimitTier":    "default",
		},
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, lockPath := range []string{
			filepath.Join(filepath.Dir(path), ".oauth_refresh.lock"),
			filepath.Clean(filepath.Dir(path)) + ".lock",
		} {
			if _, err := os.Stat(lockPath); err != nil {
				t.Errorf("Claude-compatible lock %s is absent during refresh: %v", lockPath, err)
			}
		}
		if r.URL.Path != "/v1/oauth/token" {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if body["grant_type"] != "refresh_token" || body["refresh_token"] != "old-refresh" {
			t.Errorf("unexpected refresh request: %#v", body)
		}
		if body["client_id"] != claudeOAuthClientID {
			t.Errorf("client_id = %q", body["client_id"])
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "new-access",
			"refresh_token": "new-refresh",
			"expires_in":    3600,
			"scope":         "user:inference user:profile",
		})
	}))
	defer server.Close()

	source := newClaudeOAuthSource(path)
	source.client = server.Client()
	source.tokenURL = server.URL + "/v1/oauth/token"
	source.now = func() time.Time { return now }

	token, err := source.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if token.access != "new-access" {
		t.Fatalf("access token was not refreshed")
	}

	var saved struct {
		Claude struct {
			Access  string `json:"accessToken"`
			Refresh string `json:"refreshToken"`
			Expires int64  `json:"expiresAt"`
		} `json:"claudeAiOauth"`
	}
	readJSONFile(t, path, &saved)
	if saved.Claude.Access != "new-access" || saved.Claude.Refresh != "new-refresh" {
		t.Fatal("rotated Claude tokens were not persisted")
	}
	if saved.Claude.Expires != now.Add(time.Hour).UnixMilli() {
		t.Fatalf("expiresAt = %d", saved.Claude.Expires)
	}
	assertPrivateFile(t, path)
}

func TestOpenAICodexOAuthRefreshesAndExtractsAccount(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	path := filepath.Join(t.TempDir(), "openai-codex.json")
	writeJSONFile(t, path, map[string]any{
		"access": "expired-access", "refresh": "old-refresh", "expires": now.Add(-time.Minute).UnixMilli(),
	})
	access := fakeCodexJWT(t, "account-123")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "old-refresh" {
			t.Errorf("unexpected refresh form: %v", r.Form)
		}
		if r.Form.Get("client_id") != openAICodexClientID {
			t.Errorf("client_id = %q", r.Form.Get("client_id"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": access, "refresh_token": "new-refresh", "expires_in": 3600,
		})
	}))
	defer server.Close()

	source := newOpenAICodexOAuthSource(path)
	source.client = server.Client()
	source.tokenURL = server.URL
	source.now = func() time.Time { return now }

	token, err := source.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if token.access != access || token.accountID != "account-123" {
		t.Fatalf("unexpected token metadata: access matched=%v account=%q", token.access == access, token.accountID)
	}

	var saved oauthCredential
	readJSONFile(t, path, &saved)
	if saved.Refresh != "new-refresh" || saved.AccountID != "account-123" {
		t.Fatal("rotated OpenAI credential was not persisted")
	}
	assertPrivateFile(t, path)
}

func TestOpenAICodexOAuthSerializesRefreshAcrossSources(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	path := filepath.Join(t.TempDir(), "openai-codex.json")
	writeJSONFile(t, path, map[string]any{
		"access": "expired-access", "refresh": "one-use-refresh", "expires": now.Add(-time.Minute).UnixMilli(),
	})
	access := fakeCodexJWT(t, "account-serialized")
	var refreshCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshCalls.Add(1)
		time.Sleep(100 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": access, "refresh_token": "rotated-refresh", "expires_in": 3600,
		})
	}))
	defer server.Close()

	sources := []*openAICodexOAuthSource{
		newOpenAICodexOAuthSource(path),
		newOpenAICodexOAuthSource(path),
	}
	for _, source := range sources {
		source.client = server.Client()
		source.tokenURL = server.URL
		source.now = func() time.Time { return now }
	}
	start := make(chan struct{})
	errors := make(chan error, len(sources))
	var wg sync.WaitGroup
	for _, source := range sources {
		wg.Add(1)
		go func(source *openAICodexOAuthSource) {
			defer wg.Done()
			<-start
			_, err := source.Token(context.Background())
			errors <- err
		}(source)
	}
	close(start)
	wg.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("refresh calls = %d, want 1", got)
	}
}

func TestDirectoryLockHeartbeatsAndDoesNotRemoveSuccessor(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "credential.lock")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	lock, err := acquireDirectoryLock(ctx, lockPath, 200*time.Millisecond, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(350 * time.Millisecond)
	secondCtx, secondCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer secondCancel()
	if second, err := acquireDirectoryLock(secondCtx, lockPath, 200*time.Millisecond, 50*time.Millisecond); err == nil {
		second.Release()
		t.Fatal("heartbeat did not prevent stale-lock takeover")
	}

	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(lockPath, 0700); err != nil {
		t.Fatal(err)
	}
	lock.Release()
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("releasing former owner removed successor lock: %v", err)
	}
}

func TestOpenAICodexDeviceLoginSavesCredentialWithoutPrintingIt(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	path := filepath.Join(t.TempDir(), "openai-codex.json")
	access := fakeCodexJWT(t, "account-device")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/accounts/deviceauth/usercode":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_auth_id": "device-id", "user_code": "ABCD-EFGH", "interval": 0,
			})
		case "/api/accounts/deviceauth/token":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"authorization_code": "authorization-code", "code_verifier": "verifier",
			})
		case "/oauth/token":
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			if r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code") != "authorization-code" {
				t.Errorf("unexpected exchange form: %v", r.Form)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": access, "refresh_token": "device-refresh", "expires_in": 3600,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	source := newOpenAICodexOAuthSource(path)
	source.client = server.Client()
	source.deviceUserCodeURL = server.URL + "/api/accounts/deviceauth/usercode"
	source.deviceTokenURL = server.URL + "/api/accounts/deviceauth/token"
	source.deviceVerificationURL = server.URL + "/codex/device"
	source.tokenURL = server.URL + "/oauth/token"
	source.now = func() time.Time { return now }

	var output strings.Builder
	if err := source.Login(context.Background(), &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "ABCD-EFGH") || !strings.Contains(output.String(), source.deviceVerificationURL) {
		t.Fatalf("login instructions missing: %q", output.String())
	}
	if strings.Contains(output.String(), access) || strings.Contains(output.String(), "device-refresh") {
		t.Fatal("login output exposed a credential")
	}

	var saved oauthCredential
	readJSONFile(t, path, &saved)
	if saved.Access != access || saved.Refresh != "device-refresh" || saved.AccountID != "account-device" {
		t.Fatal("device credential was not saved")
	}
	assertPrivateFile(t, path)
}

func fakeCodexJWT(t *testing.T, accountID string) string {
	t.Helper()
	encode := func(value any) string {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(data)
	}
	return encode(map[string]any{"alg": "none"}) + "." + encode(map[string]any{
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": accountID},
	}) + ".signature"
}

func writeJSONFile(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func readJSONFile(t *testing.T, path string, value any) {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	if err := json.NewDecoder(file).Decode(value); err != nil && err != io.EOF {
		t.Fatal(err)
	}
}

func assertPrivateFile(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}
}
