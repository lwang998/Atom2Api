package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSemanticVersionLess(t *testing.T) {
	tests := []struct {
		name    string
		current string
		latest  string
		want    bool
	}{
		{name: "patch update", current: "1.0.2", latest: "v1.0.3", want: true},
		{name: "minor update", current: "1.9.9", latest: "2.0.0", want: true},
		{name: "prerelease to stable", current: "1.2.0-rc.1", latest: "1.2.0", want: true},
		{name: "same version", current: "v1.2.0", latest: "1.2.0", want: false},
		{name: "build metadata ignored", current: "1.2.0+local", latest: "1.2.0+release", want: false},
		{name: "running version newer", current: "1.3.0", latest: "1.2.9", want: false},
		{name: "development build", current: "dev", latest: "1.2.0", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := semanticVersionLess(test.current, test.latest); got != test.want {
				t.Fatalf("semanticVersionLess(%q, %q) = %v, want %v", test.current, test.latest, got, test.want)
			}
		})
	}
}

func TestReleaseCheckerReturnsLatestReleaseAndCaches(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if got := r.Header.Get("Accept"); got != "application/vnd.github+json" {
			t.Errorf("Accept = %q", got)
		}
		if got := r.Header.Get("User-Agent"); got != "Atom2Api/1.0.2" {
			t.Errorf("User-Agent = %q", got)
		}
		writeJSON(w, http.StatusOK, githubRelease{
			TagName: "v1.1.0", HTMLURL: "https://github.com/lwang998/Atom2Api/releases/tag/v1.1.0",
			Body: "## Changes\n\n- Added update checks", PublishedAt: "2026-08-03T01:02:03Z",
		})
	}))
	defer server.Close()

	checker := newReleaseChecker("v1.0.2", server.URL, server.Client())
	checker.now = func() time.Time { return time.Date(2026, 8, 3, 2, 3, 4, 0, time.UTC) }

	response := httptest.NewRecorder()
	checker.HandleVersion(response, httptest.NewRequest(http.MethodGet, "/api/version", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var info VersionInfo
	if err := json.NewDecoder(response.Body).Decode(&info); err != nil {
		t.Fatal(err)
	}
	if info.CurrentVersion != "1.0.2" || info.LatestVersion != "1.1.0" || !info.UpdateAvailable {
		t.Fatalf("version info = %#v", info)
	}
	if info.RepositoryURL != githubRepositoryURL {
		t.Fatalf("repository URL = %q, want %q", info.RepositoryURL, githubRepositoryURL)
	}
	if info.ReleaseNotes != "## Changes\n\n- Added update checks" || info.CheckedAt != "2026-08-03T02:03:04Z" {
		t.Fatalf("release details = %#v", info)
	}

	_ = checker.Check(context.Background())
	if requests != 1 {
		t.Fatalf("GitHub requests = %d, want 1", requests)
	}

	response = httptest.NewRecorder()
	checker.HandleVersion(response, httptest.NewRequest(http.MethodGet, "/api/version?refresh=1", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("refresh status = %d, body = %s", response.Code, response.Body.String())
	}
	if requests != 2 {
		t.Fatalf("GitHub requests after refresh = %d, want 2", requests)
	}
}

func TestReleaseCheckerReportsFailureWithoutHidingCurrentVersion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	checker := newReleaseChecker("1.0.2", server.URL, server.Client())
	info := checker.Check(context.Background())
	if info.CurrentVersion != "1.0.2" || info.RepositoryURL != githubRepositoryURL || info.CheckError == "" {
		t.Fatalf("version info = %#v", info)
	}
	if info.UpdateAvailable || info.LatestVersion != "" {
		t.Fatalf("failed check must not report an update: %#v", info)
	}
}
