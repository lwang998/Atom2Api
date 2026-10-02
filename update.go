package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The default update-check endpoints live in update_defaults.go (overridable
// per build). Building with `-tags nogithub` compiles in empty values, which
// disables the online update check and removes every GitHub address.
const (
	releaseCheckSuccessTTL = 30 * time.Minute
	releaseCheckFailureTTL = 5 * time.Minute
)

type VersionInfo struct {
	CurrentVersion  string `json:"current_version"`
	LatestVersion   string `json:"latest_version,omitempty"`
	UpdateAvailable bool   `json:"update_available"`
	RepositoryURL   string `json:"repository_url"`
	ReleaseURL      string `json:"release_url,omitempty"`
	ReleaseNotes    string `json:"release_notes,omitempty"`
	PublishedAt     string `json:"published_at,omitempty"`
	CheckedAt       string `json:"checked_at"`
	CheckError      string `json:"check_error,omitempty"`
}

type githubRelease struct {
	TagName     string `json:"tag_name"`
	HTMLURL     string `json:"html_url"`
	Body        string `json:"body"`
	PublishedAt string `json:"published_at"`
	Draft       bool   `json:"draft"`
	Prerelease  bool   `json:"prerelease"`
}

type ReleaseChecker struct {
	currentVersion string
	releaseURL     string
	client         *http.Client
	now            func() time.Time
	successTTL     time.Duration
	failureTTL     time.Duration

	mu        sync.Mutex
	cached    VersionInfo
	expiresAt time.Time
}

func NewReleaseChecker(currentVersion string) *ReleaseChecker {
	return newReleaseChecker(currentVersion, githubLatestReleaseURL, &http.Client{Timeout: 10 * time.Second})
}

func newReleaseChecker(currentVersion, releaseURL string, client *http.Client) *ReleaseChecker {
	return &ReleaseChecker{
		currentVersion: normalizeVersionLabel(currentVersion),
		releaseURL:     releaseURL,
		client:         client,
		now:            time.Now,
		successTTL:     releaseCheckSuccessTTL,
		failureTTL:     releaseCheckFailureTTL,
	}
}

func (c *ReleaseChecker) HandleVersion(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("refresh") == "1" {
		writeJSON(w, http.StatusOK, c.Refresh(r.Context()))
		return
	}
	writeJSON(w, http.StatusOK, c.Check(r.Context()))
}

func (c *ReleaseChecker) Check(ctx context.Context) VersionInfo {
	return c.check(ctx, false)
}

func (c *ReleaseChecker) Refresh(ctx context.Context) VersionInfo {
	return c.check(ctx, true)
}

func (c *ReleaseChecker) check(ctx context.Context, force bool) VersionInfo {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now().UTC()
	if !force && !c.expiresAt.IsZero() && now.Before(c.expiresAt) {
		return c.cached
	}

	info := VersionInfo{
		CurrentVersion: c.currentVersion,
		RepositoryURL:  githubRepositoryURL,
		CheckedAt:      now.Format(time.RFC3339),
	}
	if c.releaseURL == "" {
		// Update checks are compiled out of this distribution.
		c.cached = info
		c.expiresAt = now.Add(c.successTTL)
		return info
	}
	release, err := c.fetchLatestRelease(ctx)
	ttl := c.successTTL
	if err != nil {
		info.CheckError = err.Error()
		ttl = c.failureTTL
	} else {
		info.LatestVersion = normalizeVersionLabel(release.TagName)
		info.UpdateAvailable = semanticVersionLess(info.CurrentVersion, info.LatestVersion)
		info.ReleaseURL = strings.TrimSpace(release.HTMLURL)
		info.ReleaseNotes = strings.TrimSpace(release.Body)
		info.PublishedAt = strings.TrimSpace(release.PublishedAt)
	}

	c.cached = info
	c.expiresAt = now.Add(ttl)
	return info
}

func (c *ReleaseChecker) fetchLatestRelease(ctx context.Context) (githubRelease, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.releaseURL, nil)
	if err != nil {
		return githubRelease{}, fmt.Errorf("create GitHub release request: %w", err)
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("User-Agent", "Atom2Api/"+c.currentVersion)

	response, err := c.client.Do(request)
	if err != nil {
		return githubRelease{}, fmt.Errorf("check GitHub release: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
		return githubRelease{}, fmt.Errorf("GitHub release check returned %s", response.Status)
	}

	var release githubRelease
	if err := json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&release); err != nil {
		return githubRelease{}, fmt.Errorf("decode GitHub release: %w", err)
	}
	if strings.TrimSpace(release.TagName) == "" || release.Draft || release.Prerelease {
		return githubRelease{}, fmt.Errorf("GitHub latest release response is not a stable release")
	}
	return release, nil
}

type semanticVersion struct {
	major      uint64
	minor      uint64
	patch      uint64
	prerelease []string
}

func semanticVersionLess(current, latest string) bool {
	currentVersion, currentOK := parseSemanticVersion(current)
	latestVersion, latestOK := parseSemanticVersion(latest)
	return currentOK && latestOK && compareSemanticVersions(currentVersion, latestVersion) < 0
}

func parseSemanticVersion(value string) (semanticVersion, bool) {
	value = normalizeVersionLabel(value)
	if buildIndex := strings.IndexByte(value, '+'); buildIndex >= 0 {
		value = value[:buildIndex]
	}
	parts := strings.SplitN(value, "-", 2)
	core := strings.Split(parts[0], ".")
	if len(core) != 3 {
		return semanticVersion{}, false
	}

	numbers := make([]uint64, 3)
	for index, component := range core {
		if !validNumericIdentifier(component) {
			return semanticVersion{}, false
		}
		parsed, err := strconv.ParseUint(component, 10, 64)
		if err != nil {
			return semanticVersion{}, false
		}
		numbers[index] = parsed
	}

	parsed := semanticVersion{major: numbers[0], minor: numbers[1], patch: numbers[2]}
	if len(parts) == 2 {
		parsed.prerelease = strings.Split(parts[1], ".")
		for _, identifier := range parsed.prerelease {
			if !validPrereleaseIdentifier(identifier) {
				return semanticVersion{}, false
			}
		}
	}
	return parsed, true
}

func compareSemanticVersions(left, right semanticVersion) int {
	for _, pair := range [][2]uint64{{left.major, right.major}, {left.minor, right.minor}, {left.patch, right.patch}} {
		if pair[0] < pair[1] {
			return -1
		}
		if pair[0] > pair[1] {
			return 1
		}
	}
	if len(left.prerelease) == 0 && len(right.prerelease) == 0 {
		return 0
	}
	if len(left.prerelease) == 0 {
		return 1
	}
	if len(right.prerelease) == 0 {
		return -1
	}

	limit := min(len(left.prerelease), len(right.prerelease))
	for index := 0; index < limit; index++ {
		comparison := comparePrereleaseIdentifiers(left.prerelease[index], right.prerelease[index])
		if comparison != 0 {
			return comparison
		}
	}
	if len(left.prerelease) < len(right.prerelease) {
		return -1
	}
	if len(left.prerelease) > len(right.prerelease) {
		return 1
	}
	return 0
}

func comparePrereleaseIdentifiers(left, right string) int {
	leftNumeric := allDigits(left)
	rightNumeric := allDigits(right)
	if leftNumeric && !rightNumeric {
		return -1
	}
	if !leftNumeric && rightNumeric {
		return 1
	}
	if leftNumeric && rightNumeric {
		if len(left) < len(right) {
			return -1
		}
		if len(left) > len(right) {
			return 1
		}
	}
	return strings.Compare(left, right)
}

func normalizeVersionLabel(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "dev"
	}
	if len(value) > 1 && (value[0] == 'v' || value[0] == 'V') && value[1] >= '0' && value[1] <= '9' {
		return value[1:]
	}
	return value
}

func validNumericIdentifier(value string) bool {
	return value != "" && allDigits(value) && (len(value) == 1 || value[0] != '0')
}

func validPrereleaseIdentifier(value string) bool {
	if value == "" || (allDigits(value) && len(value) > 1 && value[0] == '0') {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'A' || character > 'Z') && (character < 'a' || character > 'z') && character != '-' {
			return false
		}
	}
	return true
}

func allDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}
