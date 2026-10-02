package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestReleaseCheckerWithoutGitHubURLSkipsCheck(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		writeJSON(w, http.StatusOK, githubRelease{TagName: "v9.9.9"})
	}))
	defer server.Close()

	checker := newReleaseChecker("1.0.11-nogithub+abc1234", "", server.Client())
	checker.now = func() time.Time { return time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC) }

	response := httptest.NewRecorder()
	checker.HandleVersion(response, httptest.NewRequest(http.MethodGet, "/api/version?refresh=1", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	var info VersionInfo
	if err := json.NewDecoder(response.Body).Decode(&info); err != nil {
		t.Fatal(err)
	}
	if info.LatestVersion != "" || info.UpdateAvailable || info.CheckError != "" {
		t.Fatalf("version info = %#v, want no update data and no error", info)
	}
	if requests != 0 {
		t.Fatalf("upstream requests = %d, want 0", requests)
	}
}
