package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	adminCookieName       = "atom2api_admin"
	adminRememberCookie   = "atom2api_remember"
	adminRememberLifetime = 10 * 365 * 24 * time.Hour
	rememberCookiePurpose = "atom2api-remember-v1"
)

type adminSession struct {
	ExpiresAt time.Time
}

type loginAttempt struct {
	Count       int
	WindowStart time.Time
}

type AdminAuth struct {
	config   *ConfigManager
	mu       sync.Mutex
	sessions map[string]adminSession
	attempts map[string]loginAttempt
}

func NewAdminAuth(config *ConfigManager) *AdminAuth {
	return &AdminAuth{config: config, sessions: map[string]adminSession{}, attempts: map[string]loginAttempt{}}
}

func (a *AdminAuth) HandleLogin(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !a.allowAttempt(ip) {
		writeJSON(w, http.StatusTooManyRequests, errorResponse{Error: "too many login attempts; try again later"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var request struct {
		Password string `json:"password"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || ensureJSONEOF(decoder) != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid request body"})
		return
	}
	if !adminPasswordMatches(a.config.Snapshot().AdminPassword, request.Password) {
		a.recordFailure(ip)
		writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "invalid password"})
		return
	}
	a.clearFailures(ip)
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "could not create session"})
		return
	}
	token := base64.RawURLEncoding.EncodeToString(tokenBytes)
	expires := time.Now().Add(12 * time.Hour)
	a.mu.Lock()
	a.pruneLocked(time.Now())
	a.sessions[token] = adminSession{ExpiresAt: expires}
	a.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: adminCookieName, Value: token, Path: "/", HttpOnly: true,
		Secure: requestIsHTTPS(r), SameSite: http.SameSiteStrictMode,
		Expires: expires, MaxAge: int((12 * time.Hour).Seconds()),
	})
	http.SetCookie(w, a.rememberCookie(r, time.Now().Add(adminRememberLifetime)))
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "expires_at": expires})
}

func (a *AdminAuth) HandleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(adminCookieName); err == nil {
		a.mu.Lock()
		delete(a.sessions, cookie.Value)
		a.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{
		Name: adminCookieName, Value: "", Path: "/", HttpOnly: true,
		Secure: requestIsHTTPS(r), SameSite: http.SameSiteStrictMode,
		Expires: time.Unix(1, 0), MaxAge: -1,
	})
	http.SetCookie(w, &http.Cookie{
		Name: adminRememberCookie, Value: "", Path: "/", HttpOnly: true,
		Secure: requestIsHTTPS(r), SameSite: http.SameSiteStrictMode,
		Expires: time.Unix(1, 0), MaxAge: -1,
	})
	w.WriteHeader(http.StatusNoContent)
}

func (a *AdminAuth) HandleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"authenticated": a.Authenticated(r)})
}

func (a *AdminAuth) Authenticated(r *http.Request) bool {
	if cookie, err := r.Cookie(adminCookieName); err == nil && cookie.Value != "" {
		now := time.Now()
		a.mu.Lock()
		session, ok := a.sessions[cookie.Value]
		if !ok || now.After(session.ExpiresAt) {
			delete(a.sessions, cookie.Value)
			ok = false
		}
		a.mu.Unlock()
		if ok {
			return true
		}
	}
	return a.rememberCookieValid(r)
}

// rememberCookie issues a long-lived stateless login cookie so the browser
// stays signed in across server restarts. It is an HMAC over the expiry,
// keyed by the instance's encryption key and current admin password hash —
// rotating either one invalidates every remembered device.
func (a *AdminAuth) rememberCookie(r *http.Request, expires time.Time) *http.Cookie {
	return &http.Cookie{
		Name: adminRememberCookie, Value: a.signRememberToken(expires), Path: "/", HttpOnly: true,
		Secure: requestIsHTTPS(r), SameSite: http.SameSiteStrictMode,
		Expires: expires, MaxAge: int(time.Until(expires).Seconds()),
	}
}

func (a *AdminAuth) signRememberToken(expires time.Time) string {
	payload := strconv.FormatInt(expires.Unix(), 10)
	mac := hmac.New(sha256.New, a.rememberSecret())
	mac.Write([]byte("remember|" + payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (a *AdminAuth) rememberCookieValid(r *http.Request) bool {
	cookie, err := r.Cookie(adminRememberCookie)
	if err != nil || cookie.Value == "" {
		return false
	}
	payload, signature, found := strings.Cut(cookie.Value, ".")
	if !found {
		return false
	}
	expiresUnix, err := strconv.ParseInt(payload, 10, 64)
	if err != nil || time.Now().After(time.Unix(expiresUnix, 0)) {
		return false
	}
	expected, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, a.rememberSecret())
	mac.Write([]byte("remember|" + payload))
	return hmac.Equal(expected, mac.Sum(nil))
}

func (a *AdminAuth) rememberSecret() []byte {
	snapshot := a.config.Snapshot()
	mac := hmac.New(sha256.New, []byte(snapshot.EncryptionKey))
	mac.Write([]byte(rememberCookiePurpose + "|" + snapshot.AdminPassword))
	return mac.Sum(nil)
}

func (a *AdminAuth) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.Authenticated(r) {
			writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "authentication required"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *AdminAuth) allowAttempt(ip string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	attempt := a.attempts[ip]
	if now.Sub(attempt.WindowStart) > 10*time.Minute {
		delete(a.attempts, ip)
		return true
	}
	return attempt.Count < 10
}

func (a *AdminAuth) recordFailure(ip string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	attempt := a.attempts[ip]
	if attempt.WindowStart.IsZero() || now.Sub(attempt.WindowStart) > 10*time.Minute {
		attempt = loginAttempt{WindowStart: now}
	}
	attempt.Count++
	a.attempts[ip] = attempt
}

func (a *AdminAuth) clearFailures(ip string) {
	a.mu.Lock()
	delete(a.attempts, ip)
	a.mu.Unlock()
}

func (a *AdminAuth) pruneLocked(now time.Time) {
	for token, session := range a.sessions {
		if now.After(session.ExpiresAt) {
			delete(a.sessions, token)
		}
	}
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

func requestIsHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}
