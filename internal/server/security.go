package server

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	bootstrapQueryName = "capability"
	sessionCookieName  = "kanedias_session"
	capabilityBytes    = 32
	// defaultSessionTTL bounds how long a minted browser session is accepted by
	// the server, so a leaked cookie does not remain valid forever.
	defaultSessionTTL = 6 * time.Hour
)

// sessionRecord stores a session digest plus its expiry.
type sessionRecord struct {
	digest    [sha256.Size]byte
	expiresAt time.Time
}

// capabilityStore manages bootstrap and browser session capabilities.
// It stores only SHA-256 digests of issued tokens.
type capabilityStore struct {
	mu              sync.RWMutex
	random          io.Reader
	bootstrapDigest [sha256.Size]byte
	sessions        []sessionRecord
	sessionTTL      time.Duration
	output          io.Writer
}

func absoluteHTTPURL(authority, path string, query url.Values) string {
	u := url.URL{Scheme: "http", Host: authority, Path: path}
	u.RawQuery = query.Encode()
	return u.String()
}

// newCapabilityStore creates a store, generates a bootstrap token, and prints it once to output.
func newCapabilityStore(random io.Reader, output io.Writer, advertisedAddress string) (*capabilityStore, error) {
	token, digest, err := newCapability(random)
	if err != nil {
		return nil, fmt.Errorf("generate bootstrap token: %w", err)
	}
	bootstrapURL := absoluteHTTPURL(advertisedAddress, "/bootstrap", url.Values{bootstrapQueryName: {token}})
	if _, err := fmt.Fprintf(output, "Bootstrap URL: %s\n", bootstrapURL); err != nil {
		return nil, fmt.Errorf("write bootstrap token: %w", err)
	}
	return &capabilityStore{
		random:          random,
		bootstrapDigest: digest,
		sessionTTL:      defaultSessionTTL,
		output:          output,
	}, nil
}

// newCapability generates 32 random bytes, encodes them as base64url, and returns both
// the token string and its SHA-256 digest. Only the digest is retained.
func newCapability(r io.Reader) (token string, digest [sha256.Size]byte, err error) {
	raw := make([]byte, capabilityBytes)
	if _, err = io.ReadFull(r, raw); err != nil {
		return "", [sha256.Size]byte{}, fmt.Errorf("read random bytes: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(raw)
	digest = sha256.Sum256([]byte(token))
	return token, digest, nil
}

// addSession stores a new browser session digest, expiring after the store's TTL.
func (s *capabilityStore) addSession(digest [sha256.Size]byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeLocked()
	s.sessions = append(s.sessions, sessionRecord{digest: digest, expiresAt: time.Now().Add(s.sessionTTL)})
}

// purgeLocked removes expired session records. Callers must hold s.mu.
func (s *capabilityStore) purgeLocked() {
	now := time.Now()
	kept := s.sessions[:0]
	for _, rec := range s.sessions {
		if rec.expiresAt.After(now) {
			kept = append(kept, rec)
		}
	}
	s.sessions = kept
}

// validSession reports whether the token matches any unexpired stored session digest.
func (s *capabilityStore) validSession(token string) bool {
	digest := sha256.Sum256([]byte(token))
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeLocked()
	for _, rec := range s.sessions {
		if subtle.ConstantTimeCompare(digest[:], rec.digest[:]) == 1 {
			return true
		}
	}
	return false
}

// serveBootstrap handles the one-time bootstrap exchange: it validates the
// bootstrap capability query parameter and issues a browser session cookie.
func (s *capabilityStore) serveBootstrap(w http.ResponseWriter, r *http.Request) {
	provided := sha256.Sum256([]byte(r.URL.Query().Get(bootstrapQueryName)))
	// The bootstrap token is valid for the server's lifetime: any number of
	// browsers can exchange it for a session, and a server restart mints a fresh
	// token (rotating it). Sessions themselves carry a TTL and are evicted.
	if subtle.ConstantTimeCompare(provided[:], s.bootstrapDigest[:]) != 1 {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	browserToken, browserDigest, err := newCapability(s.random)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	s.addSession(browserDigest)

	// The bootstrap URL carries the capability in the query string; prevent it
	// from being cached or leaking via the Referer header on the redirect.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    browserToken,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(s.sessionTTL.Seconds()),
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// requireSession is a middleware that checks for a valid session cookie.
func (s *capabilityStore) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookieName)
		if err != nil {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		if !s.validSession(cookie.Value) {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requestBoundary enforces same-origin write boundary checks.
// Host and Origin are derived only from the listener's effective address.
type requestBoundary struct {
	Host string
}

// newRequestBoundary derives the expected Host from the effective address string.
func newRequestBoundary(effectiveAddress string) requestBoundary {
	return requestBoundary{Host: effectiveAddress}
}

// isLoopbackHost reports whether host (without port) is a loopback address:
// localhost or any loopback IP. These are all the same machine (the server is
// loopback-only), so they are interchangeable for the same-origin boundary.
func isLoopbackHost(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return strings.EqualFold(host, "localhost")
}

// boundaryHostMatches reports whether the request Host is acceptable. Specific
// addresses must match exactly, with loopback aliases allowed. An unspecified
// listener accepts any non-empty request Host on its configured port.
func boundaryHostMatches(host, expected string) bool {
	if host == expected {
		return true
	}
	expectedHost, expectedPort, err := net.SplitHostPort(expected)
	if err != nil {
		return false
	}
	h, port, err := net.SplitHostPort(host)
	if err != nil || h == "" || port != expectedPort {
		return false
	}
	if ip := net.ParseIP(expectedHost); ip != nil && ip.IsUnspecified() {
		return true
	}
	return isLoopbackHost(h) && isLoopbackHost(expectedHost)
}

// boundaryOriginMatches reports whether Origin names the request's actual Host
// and that authority is allowed by the advertised listener boundary.
func boundaryOriginMatches(origin, requestHost, expected string) bool {
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	return strings.EqualFold(u.Host, requestHost) && boundaryHostMatches(u.Host, expected)
}

// requireWriteBoundary is a middleware that enforces same-origin write constraints.
// It checks Host, Origin, Sec-Fetch-Site, and Content-Type: application/json.
func (b requestBoundary) requireWriteBoundary(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !boundaryHostMatches(r.Host, b.Host) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		origin := r.Header.Get("Origin")
		if origin != "" && !boundaryOriginMatches(origin, r.Host, b.Host) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		sfs := r.Header.Get("Sec-Fetch-Site")
		if sfs != "" && sfs != "same-origin" {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mediaType != "application/json" {
			http.Error(w, "Unsupported Media Type", http.StatusUnsupportedMediaType)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// defaultRandom is the production-grade random reader.
var defaultRandom = rand.Reader
