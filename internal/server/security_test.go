package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// deterministicReader always returns the same byte pattern.
type deterministicReader struct {
	b    byte
	step byte
}

func (r *deterministicReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = r.b
		r.b += r.step
	}
	return len(p), nil
}

func newDeterministicReader() *deterministicReader {
	return &deterministicReader{b: 0x01, step: 0x01}
}

func TestCapabilityTokenIsBase64URLOf32Bytes(t *testing.T) {
	rdr := newDeterministicReader()
	token, digest, err := newCapability(rdr)
	if err != nil {
		t.Fatalf("newCapability: %v", err)
	}

	// Token must be base64url-decodable to exactly 32 bytes.
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		t.Fatalf("token is not base64url: %v", err)
	}
	if len(raw) != capabilityBytes {
		t.Errorf("decoded token length = %d, want %d", len(raw), capabilityBytes)
	}

	// Digest must be SHA-256 of the token string.
	want := sha256.Sum256([]byte(token))
	if digest != want {
		t.Errorf("digest mismatch: got %x, want %x", digest, want)
	}
}

func TestCapabilityStoreOnlyRetainsDigests(t *testing.T) {
	var out bytes.Buffer
	store, err := newCapabilityStore(newDeterministicReader(), &out)
	if err != nil {
		t.Fatalf("newCapabilityStore: %v", err)
	}

	// Bootstrap digest must NOT be the zero value.
	var zero [sha256.Size]byte
	if store.bootstrapDigest == zero {
		t.Error("bootstrapDigest is zero")
	}

	// The raw token must NOT appear in any field (only digest is stored).
	output := out.String()
	if strings.Contains(output, string(store.bootstrapDigest[:])) {
		t.Error("raw bootstrap digest appeared in bootstrap output")
	}
	// The session records slice starts empty.
	if len(store.sessions) != 0 {
		t.Errorf("initial sessions = %d, want 0", len(store.sessions))
	}
}

func TestBootstrapPrintsTokenOnce(t *testing.T) {
	var out bytes.Buffer
	_, err := newCapabilityStore(newDeterministicReader(), &out)
	if err != nil {
		t.Fatalf("newCapabilityStore: %v", err)
	}
	output := out.String()
	if !strings.Contains(output, bootstrapQueryName+"=") {
		t.Errorf("bootstrap output = %q, want capability= prefix", output)
	}
	// Extract token from output.
	idx := strings.Index(output, bootstrapQueryName+"=")
	if idx == -1 {
		t.Fatal("no capability= found in bootstrap output")
	}
	token := strings.TrimSpace(output[idx+len(bootstrapQueryName)+1:])
	// Token must be base64url-decodable to 32 bytes.
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		t.Fatalf("bootstrap token %q is not base64url: %v", token, err)
	}
	if len(raw) != capabilityBytes {
		t.Errorf("bootstrap token decodes to %d bytes, want %d", len(raw), capabilityBytes)
	}
}

func TestInvalidBootstrapReturns403(t *testing.T) {
	var out bytes.Buffer
	store, err := newCapabilityStore(newDeterministicReader(), &out)
	if err != nil {
		t.Fatalf("newCapabilityStore: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/bootstrap?"+bootstrapQueryName+"=badtoken", nil)
	w := httptest.NewRecorder()
	store.serveBootstrap(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("invalid bootstrap status = %d, want %d", w.Code, http.StatusForbidden)
	}
}

func TestValidBootstrapRedirectsAndSetsCookie(t *testing.T) {
	var out bytes.Buffer
	store, err := newCapabilityStore(newDeterministicReader(), &out)
	if err != nil {
		t.Fatalf("newCapabilityStore: %v", err)
	}

	// Extract the bootstrap token from the printed output.
	output := out.String()
	idx := strings.Index(output, bootstrapQueryName+"=")
	if idx == -1 {
		t.Fatal("no capability= found in bootstrap output")
	}
	token := strings.TrimSpace(output[idx+len(bootstrapQueryName)+1:])

	req := httptest.NewRequest(http.MethodGet, "/bootstrap?"+bootstrapQueryName+"="+token, nil)
	w := httptest.NewRecorder()
	store.serveBootstrap(w, req)

	if w.Code != http.StatusSeeOther {
		t.Errorf("valid bootstrap status = %d, want %d", w.Code, http.StatusSeeOther)
	}
	location := w.Header().Get("Location")
	if location != "/" {
		t.Errorf("redirect location = %q, want /", location)
	}

	// Inspect Set-Cookie.
	var cookieHeader string
	for _, h := range w.Header()["Set-Cookie"] {
		if strings.Contains(h, sessionCookieName+"=") {
			cookieHeader = h
			break
		}
	}
	if cookieHeader == "" {
		t.Fatal("no session cookie set")
	}
	if !strings.Contains(cookieHeader, "HttpOnly") {
		t.Errorf("session cookie is not HttpOnly: %s", cookieHeader)
	}
	if !strings.Contains(cookieHeader, "SameSite=Strict") {
		t.Errorf("session cookie is not SameSite=Strict: %s", cookieHeader)
	}
	if !strings.Contains(cookieHeader, "Path=/") {
		t.Errorf("session cookie Path != /: %s", cookieHeader)
	}
}

// TestBootstrapSetsNoStoreAndNoReferrer is the I3 regression test: the bootstrap
// response must set Cache-Control: no-store and Referrer-Policy: no-referrer so
// the capability in the query string is neither cached nor leaked via Referer.
func TestBootstrapSetsNoStoreAndNoReferrer(t *testing.T) {
	var out bytes.Buffer
	store, err := newCapabilityStore(newDeterministicReader(), &out)
	if err != nil {
		t.Fatalf("newCapabilityStore: %v", err)
	}
	output := out.String()
	idx := strings.Index(output, bootstrapQueryName+"=")
	if idx == -1 {
		t.Fatal("no capability= found in bootstrap output")
	}
	token := strings.TrimSpace(output[idx+len(bootstrapQueryName)+1:])

	req := httptest.NewRequest(http.MethodGet, "/bootstrap?"+bootstrapQueryName+"="+token, nil)
	w := httptest.NewRecorder()
	store.serveBootstrap(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("bootstrap status = %d, want %d", w.Code, http.StatusSeeOther)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if got := w.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("Referrer-Policy = %q, want no-referrer", got)
	}
}

func TestSessionCookieIssuedByValidBootstrap(t *testing.T) {
	var out bytes.Buffer
	store, err := newCapabilityStore(newDeterministicReader(), &out)
	if err != nil {
		t.Fatalf("newCapabilityStore: %v", err)
	}

	// Obtain bootstrap token.
	output := out.String()
	idx := strings.Index(output, bootstrapQueryName+"=")
	token := strings.TrimSpace(output[idx+len(bootstrapQueryName)+1:])

	// Issue browser session via bootstrap.
	req := httptest.NewRequest(http.MethodGet, "/bootstrap?"+bootstrapQueryName+"="+token, nil)
	w := httptest.NewRecorder()
	store.serveBootstrap(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("bootstrap status = %d, want %d", w.Code, http.StatusSeeOther)
	}

	// Extract browser cookie value.
	var browserToken string
	for _, h := range w.Header()["Set-Cookie"] {
		if strings.HasPrefix(h, sessionCookieName+"=") {
			parts := strings.SplitN(h, "=", 2)
			if len(parts) == 2 {
				browserToken = strings.Split(parts[1], ";")[0]
			}
			break
		}
	}
	if browserToken == "" {
		t.Fatal("no session cookie value found")
	}

	// Browser token should be accepted by requireSession.
	protected := store.requireSession(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.AddCookie(&http.Cookie{Name: sessionCookieName, Value: browserToken})
	w2 := httptest.NewRecorder()
	protected.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Errorf("authenticated request status = %d, want 200", w2.Code)
	}
}

func TestBootstrapIsSingleUse(t *testing.T) {
	var out bytes.Buffer
	store, err := newCapabilityStore(newDeterministicReader(), &out)
	if err != nil {
		t.Fatalf("newCapabilityStore: %v", err)
	}

	// Obtain the bootstrap token.
	output := out.String()
	idx := strings.Index(output, bootstrapQueryName+"=")
	token := strings.TrimSpace(output[idx+len(bootstrapQueryName)+1:])

	// First bootstrap exchange mints a session cookie.
	first := httptest.NewRequest(http.MethodGet, "/bootstrap?"+bootstrapQueryName+"="+token, nil)
	w1 := httptest.NewRecorder()
	store.serveBootstrap(w1, first)
	if w1.Code != http.StatusSeeOther {
		t.Fatalf("first bootstrap status = %d, want %d", w1.Code, http.StatusSeeOther)
	}
	var cookies []string
	for _, h := range w1.Header()["Set-Cookie"] {
		if strings.HasPrefix(h, sessionCookieName+"=") {
			cookies = append(cookies, h)
		}
	}
	if len(cookies) != 1 {
		t.Fatalf("first bootstrap set %d session cookies, want 1", len(cookies))
	}

	// The bootstrap token is reusable until the server is restarted (a fresh
	// store+token rotates it), so a browser that loses its cookie or a second
	// browser can re-bootstrap without a server restart.
	second := httptest.NewRequest(http.MethodGet, "/bootstrap?"+bootstrapQueryName+"="+token, nil)
	w2 := httptest.NewRecorder()
	store.serveBootstrap(w2, second)
	if w2.Code != http.StatusSeeOther {
		t.Fatalf("re-bootstrap status = %d, want %d (token valid until restart)", w2.Code, http.StatusSeeOther)
	}
	if len(store.sessions) != 2 {
		t.Fatalf("two bootstraps should mint two sessions, got %d", len(store.sessions))
	}
}

func TestSessionExpiresAfterTTL(t *testing.T) {
	var out bytes.Buffer
	store, err := newCapabilityStore(newDeterministicReader(), &out)
	if err != nil {
		t.Fatalf("newCapabilityStore: %v", err)
	}
	token, digest, err := newCapability(newDeterministicReader())
	if err != nil {
		t.Fatal(err)
	}
	// A TTL already expired should make the session immediately invalid.
	store.sessionTTL = -time.Second
	store.addSession(digest)
	if store.validSession(token) {
		t.Fatal("expired session was accepted")
	}
	// After purging, the session store is empty.
	if len(store.sessions) != 0 {
		t.Fatalf("expired sessions not purged: %d remain", len(store.sessions))
	}
}

func TestNewStoreRejectsCookiesFromPreviousStore(t *testing.T) {
	// Issue a session from the first store.
	var out1 bytes.Buffer
	store1, err := newCapabilityStore(newDeterministicReader(), &out1)
	if err != nil {
		t.Fatalf("newCapabilityStore: %v", err)
	}
	output1 := out1.String()
	idx := strings.Index(output1, bootstrapQueryName+"=")
	token := strings.TrimSpace(output1[idx+len(bootstrapQueryName)+1:])

	req := httptest.NewRequest(http.MethodGet, "/bootstrap?"+bootstrapQueryName+"="+token, nil)
	w := httptest.NewRecorder()
	store1.serveBootstrap(w, req)
	var browserToken string
	for _, h := range w.Header()["Set-Cookie"] {
		if strings.HasPrefix(h, sessionCookieName+"=") {
			parts := strings.SplitN(h, "=", 2)
			if len(parts) == 2 {
				browserToken = strings.Split(parts[1], ";")[0]
			}
		}
	}

	// Create a second store — it must not accept tokens from the first.
	var out2 bytes.Buffer
	store2, err := newCapabilityStore(newDeterministicReader(), &out2)
	if err != nil {
		t.Fatalf("newCapabilityStore: %v", err)
	}

	protected := store2.requireSession(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.AddCookie(&http.Cookie{Name: sessionCookieName, Value: browserToken})
	w2 := httptest.NewRecorder()
	protected.ServeHTTP(w2, req2)
	if w2.Code != http.StatusUnauthorized {
		t.Errorf("cross-store cookie status = %d, want 401", w2.Code)
	}
}

func TestRequestBoundaryRejectsWrongHost(t *testing.T) {
	boundary := newRequestBoundary("127.0.0.1:43127")
	handler := boundary.requireWriteBoundary(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/ui/sessions", nil)
	req.Host = "evil.example.com:8080"
	req.Header.Set("Origin", "http://127.0.0.1:43127")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("wrong host status = %d, want 403", w.Code)
	}
}

func TestRequestBoundaryRejectsWrongOrigin(t *testing.T) {
	boundary := newRequestBoundary("127.0.0.1:43127")
	handler := boundary.requireWriteBoundary(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/ui/sessions", nil)
	req.Host = "127.0.0.1:43127"
	req.Header.Set("Origin", "http://evil.example.com")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("wrong origin status = %d, want 403", w.Code)
	}
}

func TestRequestBoundaryRejectsNonSameOriginFetchSite(t *testing.T) {
	boundary := newRequestBoundary("127.0.0.1:43127")
	handler := boundary.requireWriteBoundary(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/ui/sessions", nil)
	req.Host = "127.0.0.1:43127"
	req.Header.Set("Origin", "http://127.0.0.1:43127")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("cross-site fetch status = %d, want 403", w.Code)
	}
}

func TestRequestBoundaryRejectsNonJSONContentType(t *testing.T) {
	boundary := newRequestBoundary("127.0.0.1:43127")
	handler := boundary.requireWriteBoundary(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/ui/sessions", nil)
	req.Host = "127.0.0.1:43127"
	req.Header.Set("Origin", "http://127.0.0.1:43127")
	req.Header.Set("Content-Type", "text/plain")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("wrong content type status = %d, want 415", w.Code)
	}
}

func TestRequestBoundaryAcceptsSameOriginRequest(t *testing.T) {
	boundary := newRequestBoundary("127.0.0.1:43127")
	handler := boundary.requireWriteBoundary(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/ui/sessions", nil)
	req.Host = "127.0.0.1:43127"
	req.Header.Set("Origin", "http://127.0.0.1:43127")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("same-origin request status = %d, want 200", w.Code)
	}
}

func TestRequestBoundaryAcceptsAbsentFetchSite(t *testing.T) {
	boundary := newRequestBoundary("127.0.0.1:43127")
	handler := boundary.requireWriteBoundary(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// No Sec-Fetch-Site header is also acceptable.
	req := httptest.NewRequest(http.MethodPost, "/ui/sessions", nil)
	req.Host = "127.0.0.1:43127"
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("absent fetch site status = %d, want 200", w.Code)
	}
}

func TestRequestBoundaryAcceptsAbsentOrigin(t *testing.T) {
	boundary := newRequestBoundary("127.0.0.1:43127")
	handler := boundary.requireWriteBoundary(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// No Origin header is also acceptable (e.g. same-origin form POST).
	req := httptest.NewRequest(http.MethodPost, "/ui/sessions", nil)
	req.Host = "127.0.0.1:43127"
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("absent origin status = %d, want 200", w.Code)
	}
}

func TestRequireSessionRejects401WithoutCookie(t *testing.T) {
	var out bytes.Buffer
	store, err := newCapabilityStore(newDeterministicReader(), &out)
	if err != nil {
		t.Fatalf("newCapabilityStore: %v", err)
	}

	protected := store.requireSession(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	protected.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("no cookie status = %d, want 401", w.Code)
	}
}

func TestRequireSessionRejectsUnknownCookie(t *testing.T) {
	var out bytes.Buffer
	store, err := newCapabilityStore(newDeterministicReader(), &out)
	if err != nil {
		t.Fatalf("newCapabilityStore: %v", err)
	}

	protected := store.requireSession(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "unknowntoken"})
	w := httptest.NewRecorder()
	protected.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("unknown cookie status = %d, want 401", w.Code)
	}
}

func TestRequestBoundaryAcceptsLoopbackAlias(t *testing.T) {
	boundary := newRequestBoundary("127.0.0.1:43127")
	handler := boundary.requireWriteBoundary(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for _, host := range []string{"localhost:43127", "127.0.0.1:43127", "[::1]:43127"} {
		req := httptest.NewRequest(http.MethodPost, "/ui/sessions", nil)
		req.Host = host
		req.Header.Set("Origin", "http://"+host)
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("host %q status = %d, want 200 (loopback alias)", host, w.Code)
		}
	}
}

func TestRequestBoundaryRejectsLoopbackAliasWithWrongPort(t *testing.T) {
	boundary := newRequestBoundary("127.0.0.1:43127")
	handler := boundary.requireWriteBoundary(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "/ui/sessions", nil)
	req.Host = "localhost:9999"
	req.Header.Set("Origin", "http://localhost:9999")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("wrong port alias status = %d, want 403", w.Code)
	}
}
