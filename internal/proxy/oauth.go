package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	claudeOAuthClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	claudeTokenURL      = "https://platform.claude.com/v1/oauth/token"

	openAICodexClientID              = "app_EMoamEEZ73f0CkXaXp7hrann"
	openAICodexTokenURL              = "https://auth.openai.com/oauth/token"
	openAICodexDeviceUserCodeURL     = "https://auth.openai.com/api/accounts/deviceauth/usercode"
	openAICodexDeviceTokenURL        = "https://auth.openai.com/api/accounts/deviceauth/token"
	openAICodexDeviceVerificationURL = "https://auth.openai.com/codex/device"
	openAICodexDeviceRedirectURI     = "https://auth.openai.com/deviceauth/callback"
)

const (
	oauthExpirySkew               = 5 * time.Minute
	claudeLockStale               = 10 * time.Second
	claudeLockHeartbeat           = 5 * time.Second
	openAICredentialLockStale     = 30 * time.Second
	openAICredentialLockHeartbeat = 10 * time.Second
)

type bearerToken struct {
	access    string
	accountID string
}

type bearerTokenSource interface {
	Token(context.Context) (bearerToken, error)
}

type bearerTokenSourceFunc func(context.Context) (bearerToken, error)

func (f bearerTokenSourceFunc) Token(ctx context.Context) (bearerToken, error) {
	return f(ctx)
}

type oauthCredential struct {
	Access    string `json:"access"`
	Refresh   string `json:"refresh"`
	Expires   int64  `json:"expires"`
	AccountID string `json:"accountId,omitempty"`
}

type openAICodexOAuthSource struct {
	mu                    sync.Mutex
	path                  string
	client                *http.Client
	now                   func() time.Time
	tokenURL              string
	deviceUserCodeURL     string
	deviceTokenURL        string
	deviceVerificationURL string
	deviceRedirectURI     string
}

func newOpenAICodexOAuthSource(path string) *openAICodexOAuthSource {
	return &openAICodexOAuthSource{
		path:                  path,
		client:                directHTTPClient(),
		now:                   time.Now,
		tokenURL:              openAICodexTokenURL,
		deviceUserCodeURL:     openAICodexDeviceUserCodeURL,
		deviceTokenURL:        openAICodexDeviceTokenURL,
		deviceVerificationURL: openAICodexDeviceVerificationURL,
		deviceRedirectURI:     openAICodexDeviceRedirectURI,
	}
}

func (s *openAICodexOAuthSource) Token(ctx context.Context) (bearerToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return bearerToken{}, err
	}
	lockCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	lock, err := acquireDirectoryLock(lockCtx, s.path+".lock", openAICredentialLockStale, openAICredentialLockHeartbeat)
	if err != nil {
		return bearerToken{}, fmt.Errorf("acquire OpenAI Codex credential lock: %w", err)
	}
	defer lock.Release()

	credential, err := readOAuthCredential(s.path)
	if err != nil {
		return bearerToken{}, fmt.Errorf("read OpenAI Codex OAuth credential: %w", err)
	}
	if credential.Expires <= s.now().Add(oauthExpirySkew).UnixMilli() {
		credential, err = s.refresh(ctx, credential, lock)
		if err != nil {
			return bearerToken{}, err
		}
	}
	if credential.AccountID == "" {
		credential.AccountID, err = codexAccountID(credential.Access)
		if err != nil {
			return bearerToken{}, err
		}
		if err := lock.Err(); err != nil {
			return bearerToken{}, err
		}
		if err := atomicWriteJSON(s.path, credential); err != nil {
			return bearerToken{}, fmt.Errorf("save OpenAI Codex OAuth credential: %w", err)
		}
	}
	return bearerToken{access: credential.Access, accountID: credential.AccountID}, nil
}

func (s *openAICodexOAuthSource) refresh(ctx context.Context, current oauthCredential, lock *directoryLock) (oauthCredential, error) {
	if current.Refresh == "" {
		return oauthCredential{}, errors.New("OpenAI Codex OAuth refresh token is missing")
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {current.Refresh},
		"client_id":     {openAICodexClientID},
	}
	credential, err := s.requestToken(ctx, form, current.Refresh)
	if err != nil {
		return oauthCredential{}, fmt.Errorf("refresh OpenAI Codex OAuth token: %w", err)
	}
	if err := lock.Err(); err != nil {
		return oauthCredential{}, err
	}
	if err := atomicWriteJSON(s.path, credential); err != nil {
		return oauthCredential{}, fmt.Errorf("save OpenAI Codex OAuth credential: %w", err)
	}
	return credential, nil
}

func (s *openAICodexOAuthSource) requestToken(ctx context.Context, form url.Values, fallbackRefresh string) (oauthCredential, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return oauthCredential{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.client.Do(req)
	if err != nil {
		return oauthCredential{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return oauthCredential{}, fmt.Errorf("token endpoint returned HTTP %d", resp.StatusCode)
	}
	var payload struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		return oauthCredential{}, fmt.Errorf("decode token response: %w", err)
	}
	if payload.AccessToken == "" || payload.ExpiresIn <= 0 {
		return oauthCredential{}, errors.New("token response is missing required fields")
	}
	if payload.RefreshToken == "" {
		payload.RefreshToken = fallbackRefresh
	}
	accountID, err := codexAccountID(payload.AccessToken)
	if err != nil {
		return oauthCredential{}, err
	}
	return oauthCredential{
		Access:    payload.AccessToken,
		Refresh:   payload.RefreshToken,
		Expires:   s.now().Add(time.Duration(payload.ExpiresIn) * time.Second).UnixMilli(),
		AccountID: accountID,
	}, nil
}

func (s *openAICodexOAuthSource) Login(ctx context.Context, output io.Writer) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	requestBody, err := json.Marshal(map[string]string{"client_id": openAICodexClientID})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.deviceUserCodeURL, bytes.NewReader(requestBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("request OpenAI device code: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("OpenAI device-code endpoint returned HTTP %d", resp.StatusCode)
	}
	var device struct {
		DeviceAuthID string          `json:"device_auth_id"`
		UserCode     string          `json:"user_code"`
		Interval     json.RawMessage `json:"interval"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&device); err != nil {
		return fmt.Errorf("decode OpenAI device code: %w", err)
	}
	interval, err := parseInterval(device.Interval)
	if err != nil || device.DeviceAuthID == "" || device.UserCode == "" {
		return errors.New("OpenAI device-code response is missing required fields")
	}
	_, _ = fmt.Fprintf(output, "Open %s and enter code %s\n", s.deviceVerificationURL, device.UserCode)

	loginCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	code, verifier, err := s.pollDeviceAuthorization(loginCtx, device.DeviceAuthID, device.UserCode, interval)
	if err != nil {
		return err
	}
	credential, err := s.requestToken(loginCtx, url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {openAICodexClientID},
		"code":          {code},
		"code_verifier": {verifier},
		"redirect_uri":  {s.deviceRedirectURI},
	}, "")
	if err != nil {
		return fmt.Errorf("exchange OpenAI device authorization: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	lock, err := acquireDirectoryLock(loginCtx, s.path+".lock", openAICredentialLockStale, openAICredentialLockHeartbeat)
	if err != nil {
		return fmt.Errorf("acquire OpenAI Codex credential lock: %w", err)
	}
	defer lock.Release()
	if err := lock.Err(); err != nil {
		return err
	}
	if err := atomicWriteJSON(s.path, credential); err != nil {
		return fmt.Errorf("save OpenAI Codex OAuth credential: %w", err)
	}
	_, _ = fmt.Fprintln(output, "OpenAI Codex login complete.")
	return nil
}

func (s *openAICodexOAuthSource) pollDeviceAuthorization(ctx context.Context, deviceID, userCode string, interval time.Duration) (string, string, error) {
	for {
		if interval > 0 {
			timer := time.NewTimer(interval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return "", "", errors.New("OpenAI device authorization timed out")
			case <-timer.C:
			}
		}
		body, err := json.Marshal(map[string]string{"device_auth_id": deviceID, "user_code": userCode})
		if err != nil {
			return "", "", err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.deviceTokenURL, bytes.NewReader(body))
		if err != nil {
			return "", "", err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := s.client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return "", "", errors.New("OpenAI device authorization timed out")
			}
			return "", "", fmt.Errorf("poll OpenAI device authorization: %w", err)
		}
		responseBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		if readErr != nil {
			return "", "", readErr
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			var result struct {
				AuthorizationCode string `json:"authorization_code"`
				CodeVerifier      string `json:"code_verifier"`
			}
			if err := json.Unmarshal(responseBody, &result); err != nil {
				return "", "", fmt.Errorf("decode OpenAI device authorization: %w", err)
			}
			if result.AuthorizationCode == "" || result.CodeVerifier == "" {
				return "", "", errors.New("OpenAI device authorization response is missing required fields")
			}
			return result.AuthorizationCode, result.CodeVerifier, nil
		}
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
			continue
		}
		var failure struct {
			Error any `json:"error"`
		}
		_ = json.Unmarshal(responseBody, &failure)
		code := oauthErrorCode(failure.Error)
		switch code {
		case "deviceauth_authorization_pending", "authorization_pending":
			continue
		case "slow_down":
			interval += 5 * time.Second
			continue
		default:
			return "", "", fmt.Errorf("OpenAI device authorization returned HTTP %d", resp.StatusCode)
		}
	}
}

func oauthErrorCode(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case map[string]any:
		code, _ := typed["code"].(string)
		return code
	default:
		return ""
	}
}

func parseInterval(raw json.RawMessage) (time.Duration, error) {
	if len(raw) == 0 {
		return 5 * time.Second, nil
	}
	var number float64
	if err := json.Unmarshal(raw, &number); err != nil {
		var text string
		if stringErr := json.Unmarshal(raw, &text); stringErr != nil {
			return 0, err
		}
		parsed, parseErr := strconv.ParseFloat(strings.TrimSpace(text), 64)
		if parseErr != nil {
			return 0, parseErr
		}
		number = parsed
	}
	if number < 0 {
		return 0, errors.New("negative polling interval")
	}
	return time.Duration(number * float64(time.Second)), nil
}

func codexAccountID(accessToken string) (string, error) {
	parts := strings.Split(accessToken, ".")
	if len(parts) != 3 {
		return "", errors.New("OpenAI Codex access token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", errors.New("decode OpenAI Codex access token")
	}
	var claims map[string]json.RawMessage
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", errors.New("decode OpenAI Codex access-token claims")
	}
	var auth struct {
		AccountID string `json:"chatgpt_account_id"`
	}
	if err := json.Unmarshal(claims["https://api.openai.com/auth"], &auth); err != nil || auth.AccountID == "" {
		return "", errors.New("OpenAI Codex access token has no ChatGPT account ID")
	}
	return auth.AccountID, nil
}

func readOAuthCredential(path string) (oauthCredential, error) {
	var credential oauthCredential
	if err := readJSON(path, &credential); err != nil {
		return oauthCredential{}, err
	}
	if credential.Access == "" {
		return oauthCredential{}, errors.New("access token is missing")
	}
	return credential, nil
}

type claudeCredentialFile struct {
	ClaudeAI claudeOAuthCredential `json:"claudeAiOauth"`
}

type claudeOAuthCredential struct {
	AccessToken      string   `json:"accessToken"`
	RefreshToken     string   `json:"refreshToken"`
	ExpiresAt        int64    `json:"expiresAt"`
	Scopes           []string `json:"scopes,omitempty"`
	SubscriptionType string   `json:"subscriptionType,omitempty"`
	RateLimitTier    string   `json:"rateLimitTier,omitempty"`
	ClientID         string   `json:"clientId,omitempty"`
}

type claudeOAuthSource struct {
	mu       sync.Mutex
	path     string
	client   *http.Client
	now      func() time.Time
	tokenURL string
}

func newClaudeOAuthSource(path string) *claudeOAuthSource {
	return &claudeOAuthSource{path: path, client: directHTTPClient(), now: time.Now, tokenURL: claudeTokenURL}
}

func (s *claudeOAuthSource) Token(ctx context.Context) (bearerToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	file, err := readClaudeCredential(s.path)
	if err != nil {
		return bearerToken{}, fmt.Errorf("read Claude OAuth credential: %w", err)
	}
	if file.ClaudeAI.ExpiresAt > s.now().Add(oauthExpirySkew).UnixMilli() {
		return bearerToken{access: file.ClaudeAI.AccessToken}, nil
	}

	lockCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	locks, err := acquireClaudeCredentialLocks(lockCtx, filepath.Dir(s.path))
	if err != nil {
		return bearerToken{}, fmt.Errorf("acquire Claude OAuth refresh lock: %w", err)
	}
	defer releaseDirectoryLocks(locks)

	file, err = readClaudeCredential(s.path)
	if err != nil {
		return bearerToken{}, fmt.Errorf("re-read Claude OAuth credential: %w", err)
	}
	if file.ClaudeAI.ExpiresAt <= s.now().Add(oauthExpirySkew).UnixMilli() {
		if err := s.refresh(ctx, &file); err != nil {
			return bearerToken{}, err
		}
		if err := directoryLocksError(locks); err != nil {
			return bearerToken{}, err
		}
		if err := atomicWriteJSON(s.path, file); err != nil {
			return bearerToken{}, fmt.Errorf("save Claude OAuth credential: %w", err)
		}
	}
	return bearerToken{access: file.ClaudeAI.AccessToken}, nil
}

func (s *claudeOAuthSource) refresh(ctx context.Context, file *claudeCredentialFile) error {
	credential := &file.ClaudeAI
	if credential.RefreshToken == "" {
		return errors.New("missing Claude OAuth refresh token")
	}
	clientID := credential.ClientID
	if clientID == "" {
		clientID = claudeOAuthClientID
	}
	requestBody, err := json.Marshal(map[string]any{
		"grant_type":    "refresh_token",
		"refresh_token": credential.RefreshToken,
		"client_id":     clientID,
		"scope":         strings.Join(credential.Scopes, " "),
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.tokenURL, bytes.NewReader(requestBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("refresh Claude OAuth token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("token endpoint returned HTTP %d for Claude OAuth", resp.StatusCode)
	}
	var payload struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Scope        string `json:"scope"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		return fmt.Errorf("decode Claude OAuth token response: %w", err)
	}
	if payload.AccessToken == "" || payload.ExpiresIn <= 0 {
		return errors.New("missing required fields in Claude OAuth token response")
	}
	credential.AccessToken = payload.AccessToken
	if payload.RefreshToken != "" {
		credential.RefreshToken = payload.RefreshToken
	}
	credential.ExpiresAt = s.now().Add(time.Duration(payload.ExpiresIn) * time.Second).UnixMilli()
	if scopes := strings.Fields(payload.Scope); len(scopes) > 0 {
		credential.Scopes = scopes
	}
	return nil
}

func readClaudeCredential(path string) (claudeCredentialFile, error) {
	var file claudeCredentialFile
	if err := readJSON(path, &file); err != nil {
		return claudeCredentialFile{}, err
	}
	if file.ClaudeAI.AccessToken == "" {
		return claudeCredentialFile{}, errors.New("access token is missing")
	}
	return file, nil
}

type directoryLock struct {
	path string
	info os.FileInfo
	stop chan struct{}
	done chan struct{}
	once sync.Once
	mu   sync.Mutex
	err  error
}

func acquireDirectoryLock(ctx context.Context, path string, stale, heartbeat time.Duration) (*directoryLock, error) {
	for {
		if err := os.Mkdir(path, 0700); err == nil {
			info, statErr := os.Stat(path)
			if statErr != nil {
				_ = os.Remove(path)
				return nil, statErr
			}
			lock := &directoryLock{path: path, info: info, stop: make(chan struct{}), done: make(chan struct{})}
			go lock.heartbeat(heartbeat)
			return lock, nil
		} else if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		if info, err := os.Stat(path); err == nil && time.Since(info.ModTime()) > stale {
			_ = os.Remove(path)
			continue
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (l *directoryLock) heartbeat(interval time.Duration) {
	defer close(l.done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-l.stop:
			return
		case now := <-ticker.C:
			info, err := os.Stat(l.path)
			if err != nil || !os.SameFile(l.info, info) {
				l.setError(errors.New("credential lock ownership was lost"))
				return
			}
			if err := os.Chtimes(l.path, now, now); err != nil {
				l.setError(fmt.Errorf("refresh credential lock heartbeat: %w", err))
				return
			}
		}
	}
}

func (l *directoryLock) setError(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.err = err
}

func (l *directoryLock) Err() error {
	l.mu.Lock()
	err := l.err
	l.mu.Unlock()
	if err != nil {
		return err
	}
	info, statErr := os.Stat(l.path)
	if statErr != nil || !os.SameFile(l.info, info) {
		err = errors.New("credential lock ownership was lost")
		l.setError(err)
		return err
	}
	return nil
}

func (l *directoryLock) Release() {
	l.once.Do(func() {
		close(l.stop)
		<-l.done
		info, err := os.Stat(l.path)
		if err == nil && os.SameFile(l.info, info) {
			_ = os.Remove(l.path)
		}
	})
}

func acquireClaudeCredentialLocks(ctx context.Context, configDir string) ([]*directoryLock, error) {
	realConfigDir, err := filepath.EvalSymlinks(configDir)
	if err != nil {
		return nil, err
	}
	paths := []string{
		filepath.Join(configDir, ".oauth_refresh.lock"),
		realConfigDir + ".lock",
	}
	locks := make([]*directoryLock, 0, len(paths))
	for _, path := range paths {
		lock, err := acquireDirectoryLock(ctx, path, claudeLockStale, claudeLockHeartbeat)
		if err != nil {
			releaseDirectoryLocks(locks)
			return nil, err
		}
		locks = append(locks, lock)
	}
	return locks, nil
}

func releaseDirectoryLocks(locks []*directoryLock) {
	for index := len(locks) - 1; index >= 0; index-- {
		locks[index].Release()
	}
}

func directoryLocksError(locks []*directoryLock) error {
	for _, lock := range locks {
		if err := lock.Err(); err != nil {
			return err
		}
	}
	return nil
}

func readJSON(path string, value any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	if err := json.NewDecoder(io.LimitReader(file, 1<<20)).Decode(value); err != nil {
		return err
	}
	return nil
}

func atomicWriteJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".oauth-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0600); err != nil {
		_ = temporary.Close()
		return err
	}
	encoder := json.NewEncoder(temporary)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return os.Chmod(path, 0600)
}

func directHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		},
	}
}
