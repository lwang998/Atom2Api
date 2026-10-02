package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func TestConfigManagerCreatesDefaultConfig(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	manager, err := NewConfigManager(configPath)
	if err != nil {
		t.Fatalf("NewConfigManager() error = %v", err)
	}

	if got := manager.Snapshot().UserAgent; got != defaultUserAgent {
		t.Fatalf("default UserAgent = %q, want %q", got, defaultUserAgent)
	}
	if snapshot := manager.Snapshot(); snapshot.AuditRetentionDays != 30 || snapshot.AuditDetailRetentionDays != 30 {
		t.Fatalf("default audit retention = (%d, %d), want (30, 30)", snapshot.AuditRetentionDays, snapshot.AuditDetailRetentionDays)
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read generated config: %v", err)
	}
	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatalf("generated config is invalid JSON: %v", err)
	}
	if config.UserAgent != defaultUserAgent {
		t.Fatalf("generated UserAgent = %q, want %q", config.UserAgent, defaultUserAgent)
	}
	if config.AdminPassword == defaultAdminPassword {
		t.Fatal("generated config stores the default admin password in plaintext")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(config.AdminPassword), []byte(defaultAdminPassword)); err != nil {
		t.Fatalf("generated admin password is not a matching bcrypt hash: %v", err)
	}
}

func TestConfigManagerUpgradesLegacyJSONDataPath(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"user_agent":"legacy-client","data_path":"data/custom-state.json"}`), 0o600); err != nil {
		t.Fatalf("write legacy config: %v", err)
	}
	manager, err := NewConfigManager(configPath)
	if err != nil {
		t.Fatalf("NewConfigManager: %v", err)
	}
	if got := manager.Snapshot().DataPath; got != "data/custom-state.db" {
		t.Fatalf("upgraded data path = %q", got)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read upgraded config: %v", err)
	}
	var stored Config
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatalf("decode upgraded config: %v", err)
	}
	if stored.DataPath != "data/custom-state.db" {
		t.Fatalf("stored data path = %q", stored.DataPath)
	}
}

func TestConfigManagerReloadsValidChanges(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	manager, err := NewConfigManager(configPath)
	if err != nil {
		t.Fatalf("NewConfigManager() error = %v", err)
	}

	if err := os.WriteFile(configPath, []byte("{\n  \"user_agent\": \"custom-client/1.2.3\"\n}\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	changed, err := manager.Reload()
	if err != nil {
		t.Fatalf("Reload() error = %v", err)
	}
	if !changed {
		t.Fatal("Reload() changed = false, want true")
	}
	if got := manager.Snapshot().UserAgent; got != "custom-client/1.2.3" {
		t.Fatalf("reloaded UserAgent = %q", got)
	}
}

func TestConfigManagerReloadHashesChangedAdminPassword(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	manager, err := NewConfigManager(configPath)
	if err != nil {
		t.Fatalf("NewConfigManager() error = %v", err)
	}

	config := manager.Snapshot().Config
	config.AdminPassword = "reloaded-secret"
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		t.Fatalf("encode config: %v", err)
	}
	if err := os.WriteFile(configPath, append(data, '\n'), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	changed, err := manager.Reload()
	if err != nil {
		t.Fatalf("Reload() error = %v", err)
	}
	if !changed {
		t.Fatal("Reload() changed = false, want true")
	}
	if !adminPasswordMatches(manager.Snapshot().AdminPassword, "reloaded-secret") {
		t.Fatal("runtime admin password does not match the reloaded password")
	}

	storedData, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read normalized config: %v", err)
	}
	var stored Config
	if err := json.Unmarshal(storedData, &stored); err != nil {
		t.Fatalf("decode normalized config: %v", err)
	}
	if stored.AdminPassword == "reloaded-secret" {
		t.Fatal("reloaded admin password remains in plaintext")
	}
	if !adminPasswordMatches(stored.AdminPassword, "reloaded-secret") {
		t.Fatal("stored admin password is not a matching bcrypt hash")
	}
}

func TestConfigManagerWatchesFileChanges(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	manager, err := NewConfigManager(configPath)
	if err != nil {
		t.Fatalf("NewConfigManager() error = %v", err)
	}
	manager.Start(10 * time.Millisecond)
	t.Cleanup(manager.Close)

	if err := os.WriteFile(configPath, []byte("{\n  \"user_agent\": \"watched-client/4.0\"\n}\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if manager.Snapshot().UserAgent == "watched-client/4.0" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("watched UserAgent = %q, want %q", manager.Snapshot().UserAgent, "watched-client/4.0")
}

func TestConfigManagerKeepsLastValidConfig(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	manager, err := NewConfigManager(configPath)
	if err != nil {
		t.Fatalf("NewConfigManager() error = %v", err)
	}

	if err := os.WriteFile(configPath, []byte("{\"user_agent\": \"\"}"), 0o600); err != nil {
		t.Fatalf("write invalid config: %v", err)
	}
	if changed, err := manager.Reload(); err == nil || changed {
		t.Fatalf("Reload() = (%v, %v), want (false, error)", changed, err)
	}
	if got := manager.Snapshot().UserAgent; got != defaultUserAgent {
		t.Fatalf("UserAgent after invalid reload = %q, want %q", got, defaultUserAgent)
	}
}

func TestConfigManagerUpdatePersistsImmediately(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	manager, err := NewConfigManager(configPath)
	if err != nil {
		t.Fatalf("NewConfigManager() error = %v", err)
	}

	if err := manager.Update(Config{UserAgent: "settings-page/2.0"}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if got := manager.Snapshot().UserAgent; got != "settings-page/2.0" {
		t.Fatalf("updated UserAgent = %q", got)
	}

	reloaded, err := NewConfigManager(configPath)
	if err != nil {
		t.Fatalf("reload persisted config: %v", err)
	}
	if got := reloaded.Snapshot().UserAgent; got != "settings-page/2.0" {
		t.Fatalf("persisted UserAgent = %q", got)
	}
}

func TestRetryStatusCodesNormalizeOverlapsAndAdjacentValues(t *testing.T) {
	tests := map[string]string{
		"400-500,503,429":         "400-500,503",
		"400-500,501,502,503":     "400-503",
		"503, 429, 500-502, 429":  "429,500-503",
		"400-400,401,403,402,500": "400-403,500",
	}
	for input, want := range tests {
		statuses, err := parseRetryStatusCodes(input)
		if err != nil {
			t.Fatalf("parseRetryStatusCodes(%q): %v", input, err)
		}
		if got := statuses.String(); got != want {
			t.Fatalf("parseRetryStatusCodes(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestRequestRetryConfigValidation(t *testing.T) {
	config, err := defaultConfig()
	if err != nil {
		t.Fatalf("defaultConfig: %v", err)
	}
	config.RequestRetryCount = 11
	if err := validateConfig(config); err == nil {
		t.Fatal("validateConfig accepted more than 10 retries")
	}
	config.RequestRetryCount = 1
	config.RetryStatusCodes = ""
	if err := validateConfig(config); err == nil {
		t.Fatal("validateConfig accepted retries without status codes")
	}
	config.RetryStatusCodes = "99,600,503-500"
	if _, err := normalizeConfig(&config); err == nil {
		t.Fatal("normalizeConfig accepted invalid status codes")
	}
}

func TestAuditRetentionConfigValidation(t *testing.T) {
	config, err := defaultConfig()
	if err != nil {
		t.Fatalf("defaultConfig: %v", err)
	}
	config.AuditRetentionDays = -1
	if err := validateConfig(config); err == nil {
		t.Fatal("validateConfig accepted a negative audit retention")
	}
	config.AuditRetentionDays = defaultAuditRetention
	config.AuditDetailRetentionDays = maxAuditRetention + 1
	if err := validateConfig(config); err == nil {
		t.Fatal("validateConfig accepted an excessive audit detail retention")
	}
}
