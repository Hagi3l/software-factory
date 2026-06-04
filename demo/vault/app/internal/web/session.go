package web

import (
	"net/http"
	"sync"
	"time"

	"github.com/harness-demo/vault/internal/crypto"
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

// setCookie writes the session cookie with hardened attributes.
func setCookie(w http.ResponseWriter, id string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    id,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Expires:  time.Now().Add(ttl),
	})
}

// clearCookie expires the session cookie.
func clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
}
