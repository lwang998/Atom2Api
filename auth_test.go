package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func loginAdmin(t *testing.T, auth *AdminAuth) []*http.Cookie {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"password":"atom2api"}`))
	response := httptest.NewRecorder()
	auth.HandleLogin(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("login status = %d, body = %s", response.Code, response.Body.String())
	}
	return response.Result().Cookies()
}

func cookieByName(cookies []*http.Cookie, name string) *http.Cookie {
	for _, cookie := range cookies {
		if cookie.Name == name {
			return cookie
		}
	}
	return nil
}

func authenticatedWith(t *testing.T, auth *AdminAuth, cookies ...*http.Cookie) bool {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/api/auth/status", nil)
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	return auth.Authenticated(request)
}

func TestRememberCookieGrantsAccessAcrossRestart(t *testing.T) {
	config, _ := newTestStore(t)
	auth := NewAdminAuth(config)
	cookies := loginAdmin(t, auth)
	remember := cookieByName(cookies, adminRememberCookie)
	if remember == nil {
		t.Fatal("login did not set remember cookie")
	}

	// A fresh AdminAuth simulates a server restart: in-memory sessions are gone,
	// but the remembered device must stay signed in.
	restarted := NewAdminAuth(config)
	if !authenticatedWith(t, restarted, remember) {
		t.Fatal("remember cookie rejected after simulated restart")
	}
	if remember.MaxAge < int((365 * 24 * time.Hour).Seconds()) {
		t.Fatalf("remember MaxAge = %d, want ~10 years", remember.MaxAge)
	}

	// A tampered payload must not authenticate.
	forged := *remember
	forged.Value = "1" + remember.Value
	if authenticatedWith(t, restarted, &forged) {
		t.Fatal("tampered remember cookie accepted")
	}
	// A cookie signed for a different instance must not authenticate.
	otherConfig, _ := newTestStore(t)
	if authenticatedWith(t, NewAdminAuth(otherConfig), remember) {
		t.Fatal("remember cookie accepted by a different instance")
	}
	// Session-only cookie must not leak into the remember path.
	sessionOnly := cookieByName(cookies, adminCookieName)
	if sessionOnly == nil || authenticatedWith(t, restarted, sessionOnly) {
		t.Fatal("stale session cookie authenticated after restart")
	}
}

func TestLogoutClearsRememberCookie(t *testing.T) {
	config, _ := newTestStore(t)
	auth := NewAdminAuth(config)
	cookies := loginAdmin(t, auth)
	remember := cookieByName(cookies, adminRememberCookie)
	session := cookieByName(cookies, adminCookieName)
	if remember == nil || session == nil {
		t.Fatal("login cookies missing")
	}

	logoutRequest := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	logoutRequest.AddCookie(session)
	logoutResponse := httptest.NewRecorder()
	auth.HandleLogout(logoutResponse, logoutRequest)
	if logoutResponse.Code != http.StatusNoContent {
		t.Fatalf("logout status = %d", logoutResponse.Code)
	}
	var cleared *http.Cookie
	for _, cookie := range logoutResponse.Result().Cookies() {
		if cookie.Name == adminRememberCookie {
			cleared = cookie
		}
	}
	if cleared == nil || cleared.MaxAge != -1 {
		t.Fatalf("remember cookie not cleared on logout: %#v", cleared)
	}
}

func TestPasswordChangeInvalidatesRememberedDevices(t *testing.T) {
	config, _ := newTestStore(t)
	auth := NewAdminAuth(config)
	cookies := loginAdmin(t, auth)
	remember := cookieByName(cookies, adminRememberCookie)

	updated := config.Snapshot().Config
	hashed, changed, err := normalizeAdminPassword("new-password-456")
	if err != nil || !changed {
		t.Fatalf("normalizeAdminPassword: %q changed=%v err=%v", hashed, changed, err)
	}
	updated.AdminPassword = hashed
	if err := config.Update(updated); err != nil {
		t.Fatalf("update password: %v", err)
	}

	if authenticatedWith(t, NewAdminAuth(config), remember) {
		t.Fatal("remember cookie still valid after password change")
	}
	nextAuth := NewAdminAuth(config)
	nextCookies := make([]*http.Cookie, 0, 2)
	request := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"password":"new-password-456"}`))
	response := httptest.NewRecorder()
	nextAuth.HandleLogin(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("re-login status = %d", response.Code)
	}
	nextCookies = response.Result().Cookies()
	if cookieByName(nextCookies, adminRememberCookie) == nil {
		t.Fatal("re-login did not issue a fresh remember cookie")
	}
}
