package web

import (
	"net/http"
	"sync"
	"time"

	"github.com/factory-demo/vault/internal/crypto"
)

// sessionCookie is the name of the session cookie.
const sessionCookie = "vault_session"

// session is one authenticated browser session. It holds the derived encryption key in
// memory only — the key is never persisted, so a process restart forces re-login.
type session struct {
	username string
	key      []byte // AES key derived from the master password at login
	expires  time.Time
}

// sessions is a concurrency-safe in-memory session table.
type sessions struct {
	mu   sync.Mutex
	byID map[string]session
	ttl  time.Duration
}

func newSessions(ttl time.Duration) *sessions {
	return &sessions{byID: make(map[string]session), ttl: ttl}
}

// create mints a session for username with key and returns the opaque session id.
func (s *sessions) create(username string, key []byte) (string, error) {
	id, err := crypto.RandomToken(32)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byID[id] = session{username: username, key: key, expires: time.Now().Add(s.ttl)}
	return id, nil
}

// get returns the session for id if present and unexpired.
func (s *sessions) get(id string) (session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.byID[id]
	if !ok {
		return session{}, false
	}
	if time.Now().After(sess.expires) {
		delete(s.byID, id)
		return session{}, false
	}
	return sess, true
}

// destroy removes a session.
func (s *sessions) destroy(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byID, id)
}

// cookieSameSite reports the SameSite mode (and matching Secure flag) for session cookies.
// The shipped default is None (which mandates Secure) so the cookie survives the deck's
// cross-site iframe; WithStrictCookie restores the hardened Strict posture for a standalone
// deployment. See the Server.strictCookie doc for why the embeddable mode is the default.
func (s *Server) cookieSameSite() (http.SameSite, bool) {
	if s.strictCookie {
		return http.SameSiteStrictMode, false
	}
	return http.SameSiteNoneMode, true // Secure is mandatory for SameSite=None
}

// setCookie writes the session cookie with hardened attributes.
func (s *Server) setCookie(w http.ResponseWriter, id string, ttl time.Duration) {
	sameSite, secure := s.cookieSameSite()
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    id,
		Path:     "/",
		HttpOnly: true,
		SameSite: sameSite,
		Secure:   secure,
		Expires:  time.Now().Add(ttl),
	})
}

// clearCookie expires the session cookie.
func (s *Server) clearCookie(w http.ResponseWriter) {
	sameSite, secure := s.cookieSameSite()
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: sameSite,
		Secure:   secure,
		MaxAge:   -1,
	})
}
