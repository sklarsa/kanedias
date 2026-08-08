package server

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"net/http"
	"sync"
)

const (
	bootstrapQueryName = "capability"
	sessionCookieName  = "kanedias_session"
	capabilityBytes    = 32
)

// capabilityStore manages bootstrap and browser session capabilities.
// It stores only SHA-256 digests of issued tokens.
type capabilityStore struct {
	mu              sync.RWMutex
	random          io.Reader
	bootstrapDigest [sha256.Size]byte
	sessionDigests  [][sha256.Size]byte
	output          io.Writer
}

// newCapabilityStore creates a store, generates a bootstrap token, and prints it once to output.
func newCapabilityStore(random io.Reader, output io.Writer) (*capabilityStore, error) {
	token, digest, err := newCapability(random)
	if err != nil {
		return nil, fmt.Errorf("generate bootstrap token: %w", err)
	}
	if _, err := fmt.Fprintf(output, "Bootstrap URL: /bootstrap?%s=%s\n", bootstrapQueryName, token); err != nil {
		return nil, fmt.Errorf("write bootstrap token: %w", err)
	}
	return &capabilityStore{
		random:          random,
		bootstrapDigest: digest,
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

// addSession stores a new browser session digest.
func (s *capabilityStore) addSession(digest [sha256.Size]byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessionDigests = append(s.sessionDigests, digest)
}

// validSession reports whether the token matches any stored session digest.
func (s *capabilityStore) validSession(token string) bool {
	digest := sha256.Sum256([]byte(token))
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, stored := range s.sessionDigests {
		if subtle.ConstantTimeCompare(digest[:], stored[:]) == 1 {
			return true
		}
	}
	return false
}

// serveBootstrap handles the one-time bootstrap exchange: it validates the
// bootstrap capability query parameter and issues a browser session cookie.
func (s *capabilityStore) serveBootstrap(w http.ResponseWriter, r *http.Request) {
	provided := sha256.Sum256([]byte(r.URL.Query().Get(bootstrapQueryName)))
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
	Host   string
	Origin string
}

// newRequestBoundary derives the expected Host and Origin from the effective address string.
func newRequestBoundary(effectiveAddress string) requestBoundary {
	return requestBoundary{
		Host:   effectiveAddress,
		Origin: "http://" + effectiveAddress,
	}
}

// requireWriteBoundary is a middleware that enforces same-origin write constraints.
// It checks Host, Origin, Sec-Fetch-Site, and Content-Type: application/json.
func (b requestBoundary) requireWriteBoundary(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != b.Host {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		origin := r.Header.Get("Origin")
		if origin != "" && origin != b.Origin {
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
