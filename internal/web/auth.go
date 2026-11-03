package web

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"strings"
	"time"
)

// Cookie and field names.
const (
	sessionCookie = "drs_session"
	csrfCookie    = "drs_csrf"
	csrfField     = "csrf_token"
	csrfHeader    = "X-CSRF-Token"
)

// sessionTTL is how long a session lives. It is a constant, not a knob: a
// home server does not need a sliding expiration policy.
const sessionTTL = 30 * 24 * time.Hour

// maxFormMemory bounds the body of a state-changing request.
const maxFormMemory = 1 << 20

type ctxKey int

const (
	ctxUser ctxKey = iota
	ctxCSRF
)

func randomToken() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		// The only way rand fails is the machine is out of entropy, which is
		// not something a session token can be weakened around.
		panic("no entropy for a session token: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

// sessions issues the CSRF token and loads the session.
//
// The CSRF token is issued on every response that does not already carry
// one, so a visitor sitting on the login page has one before they submit
// anything.
func (s *Server) sessions(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), ctxCSRF, s.ensureCSRF(w, r))

		if isUnsafe(r.Method) {
			// The body is bounded before anything reads it: the token check
			// below parses the form, and an unbounded body would be read
			// into memory before the handler ever sees it.
			r.Body = http.MaxBytesReader(w, r.Body, maxFormMemory)
			if !s.csrfOK(r) {
				s.log.Warn("rejected a form without a valid csrf token",
					"path", r.URL.Path, "method", r.Method, "remote", r.RemoteAddr)
				http.Error(w, "the form expired; go back and try again", http.StatusForbidden)
				return
			}
		}

		if cookie, err := r.Cookie(sessionCookie); err == nil && cookie.Value != "" {
			found, ok := s.sessionUser(cookie.Value)
			switch {
			case ok && s.sessionValid(found):
				ctx = context.WithValue(ctx, ctxUser, found.user)
			case ok:
				// The user was taken out of the config, or the password was
				// changed: the config is the list of the people who may
				// approve the moves.
				s.dropSession(cookie.Value)
				s.clearCookie(w, sessionCookie)
			default:
				// Expired or revoked: clearing it stops the browser sending
				// a dead token on every request.
				s.clearCookie(w, sessionCookie)
			}
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// requireAuth turns away a request without a session: a redirect to the
// login page for a page, an answer for the API.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.Context().Value(ctxUser).(string); !ok {
			if isAPI(r) {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "no session"})
				return
			}
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) ensureCSRF(w http.ResponseWriter, r *http.Request) string {
	if cookie, err := r.Cookie(csrfCookie); err == nil && cookie.Value != "" {
		return cookie.Value
	}
	token := randomToken()
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   s.secureCookies(),
	})
	return token
}

func (s *Server) csrfOK(r *http.Request) bool {
	cookie, err := r.Cookie(csrfCookie)
	if err != nil || cookie.Value == "" {
		return false
	}
	token := r.Header.Get(csrfHeader)
	if token == "" {
		token = r.PostFormValue(csrfField)
	}
	if token == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(cookie.Value)) == 1
}

func (s *Server) newSession(user string) string {
	token := randomToken()
	s.mu.Lock()
	s.tokens[token] = session{
		user:    user,
		hash:    passwordHash(s.cfg.Load().Web.Users[user]),
		expires: time.Now().Add(sessionTTL),
	}
	s.mu.Unlock()
	return token
}

func (s *Server) sessionUser(token string) (session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	found, ok := s.tokens[token]
	if !ok {
		return session{}, false
	}
	if time.Now().After(found.expires) {
		delete(s.tokens, token)
		return session{}, false
	}
	found.expires = time.Now().Add(sessionTTL)
	s.tokens[token] = found
	return found, true
}

func (s *Server) dropSession(token string) {
	s.mu.Lock()
	delete(s.tokens, token)
	s.mu.Unlock()
}

// checkCredentials compares the password with the one in the config.
//
// The comparison is over hashes, so it takes the same time whatever the
// length of the password, and a missing user is compared too: otherwise the
// answer would say whether the user exists.
func (s *Server) checkCredentials(name, password string) bool {
	users := s.cfg.Load().Web.Users

	want, known := users[name]
	if !known {
		want, password = "no such user", "no such password"
	}
	got := sha256.Sum256([]byte(password))
	expected := sha256.Sum256([]byte(want))
	return known && subtle.ConstantTimeCompare(got[:], expected[:]) == 1
}

// sessionValid tells whether the session may still be used.
//
// The user has to be in the configuration and the password has to be the one
// the session was made with: a user that was taken out of the config, or a
// password that was changed, must stop working at once. The config is the
// list of the people who may approve the moves.
func (s *Server) sessionValid(found session) bool {
	password, ok := s.cfg.Load().Web.Users[found.user]
	if !ok {
		return false
	}
	return found.hash == passwordHash(password)
}

// passwordHash is what a session remembers about the password.
func passwordHash(password string) string {
	sum := sha256.Sum256([]byte(password))
	return hex.EncodeToString(sum[:])
}

// secureCookies tells the browser to keep the cookies to itself over https.
func (s *Server) secureCookies() bool {
	return strings.HasPrefix(s.cfg.Load().Server.BaseURL, "https://")
}

func (s *Server) clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1})
}

func isUnsafe(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	return true
}
