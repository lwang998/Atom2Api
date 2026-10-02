package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultConfigPath       = "config.json"
	defaultUserAgent        = "atomcode/5.2.0"
	defaultListenAddress    = ":8080"
	defaultDataPath         = "data/atom2api.db"
	defaultAdminPassword    = "atom2api"
	defaultPlatformBaseURL  = "https://acs.atomgit.com"
	defaultCodingPlanAPIURL = "https://api.gitcode.com/api/v5"
	defaultGatewayURL       = "https://llm-api.atomgit.com/v1"
	defaultAuditRetention   = 30
	defaultSystemPrompt     = "You are AtomCode, an AI coding agent by AtomGit running the {model} model.\nWhen asked who or what model you are, identify yourself as AtomCode running {model}.\nNever claim to be Claude, ChatGPT, or another product, organization, or model.\nYou help users with software engineering tasks within the current project."
	maxAuditRetention       = 36500
	maxSystemPromptBytes    = 64 << 10
)

type Config struct {
	UserAgent                string `json:"user_agent"`
	ListenAddress            string `json:"listen_address"`
	DataPath                 string `json:"data_path"`
	AdminPassword            string `json:"admin_password"`
	EncryptionKey            string `json:"encryption_key"`
	PlatformBaseURL          string `json:"platform_base_url"`
	CodingPlanAPIURL         string `json:"codingplan_api_url"`
	GatewayURL               string `json:"gateway_url"`
	SignerURL                string `json:"signer_url,omitempty"`
	SignerToken              string `json:"signer_token,omitempty"`
	AuditDebugEnabled        bool   `json:"audit_debug_enabled"`
	SystemPromptEnabled      bool   `json:"system_prompt_enabled"`
	SystemPrompt             string `json:"system_prompt"`
	RequestTimeoutSecs       int    `json:"request_timeout_seconds"`
	RequestRetryCount        int    `json:"request_retry_count"`
	RetryStatusCodes         string `json:"request_retry_status_codes"`
	AuditRetentionDays       int    `json:"audit_retention_days"`
	AuditDetailRetentionDays int    `json:"audit_detail_retention_days"`
}

type retryStatusCodeSet [600]bool

func parseRetryStatusCodes(value string) (retryStatusCodeSet, error) {
	var statuses retryStatusCodeSet
	value = strings.TrimSpace(value)
	if value == "" {
		return statuses, nil
	}
	for _, rawPart := range strings.Split(value, ",") {
		part := strings.TrimSpace(rawPart)
		if part == "" {
			return retryStatusCodeSet{}, errors.New("request_retry_status_codes contains an empty item")
		}
		bounds := strings.Split(part, "-")
		if len(bounds) > 2 {
			return retryStatusCodeSet{}, fmt.Errorf("invalid retry status code range %q", part)
		}
		start, err := strconv.Atoi(strings.TrimSpace(bounds[0]))
		if err != nil || start < 100 || start > 599 {
			return retryStatusCodeSet{}, fmt.Errorf("retry status code %q must be between 100 and 599", part)
		}
		end := start
		if len(bounds) == 2 {
			end, err = strconv.Atoi(strings.TrimSpace(bounds[1]))
			if err != nil || end < 100 || end > 599 || end < start {
				return retryStatusCodeSet{}, fmt.Errorf("invalid retry status code range %q", part)
			}
		}
		for status := start; status <= end; status++ {
			statuses[status] = true
		}
	}
	return statuses, nil
}

func (statuses retryStatusCodeSet) String() string {
	parts := make([]string, 0)
	for start := 100; start <= 599; {
		if !statuses[start] {
			start++
			continue
		}
		end := start
		for end < 599 && statuses[end+1] {
			end++
		}
		if start == end {
			parts = append(parts, strconv.Itoa(start))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", start, end))
		}
		start = end + 1
	}
	return strings.Join(parts, ",")
}

func (statuses retryStatusCodeSet) Contains(status int) bool {
	return status >= 0 && status < len(statuses) && statuses[status]
}

type ConfigSnapshot struct {
	Config
	LoadedAt time.Time
}

type ConfigManager struct {
	path string

	mu       sync.RWMutex
	config   Config
	fileHash [sha256.Size]byte
	loadedAt time.Time

	startOnce sync.Once
	stopOnce  sync.Once
	stop      chan struct{}
	done      chan struct{}
}

func NewConfigManager(configPath string) (*ConfigManager, error) {
	if strings.TrimSpace(configPath) == "" {
		configPath = defaultConfigPath
	}

	manager := &ConfigManager{
		path: configPath,
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}

	data, err := os.ReadFile(configPath)
	if errors.Is(err, os.ErrNotExist) {
		config, err := defaultConfig()
		if err != nil {
			return nil, err
		}
		if err := manager.store(config); err != nil {
			return nil, fmt.Errorf("create default config: %w", err)
		}
		return manager, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	config, normalized, err := parseConfig(data)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	if normalized {
		if err := manager.store(config); err != nil {
			return nil, fmt.Errorf("upgrade config: %w", err)
		}
		return manager, nil
	}
	manager.config = config
	manager.fileHash = sha256.Sum256(data)
	manager.loadedAt = time.Now()
	return manager, nil
}

func defaultConfig() (Config, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return Config{}, fmt.Errorf("generate encryption key: %w", err)
	}
	config := Config{
		UserAgent:          defaultUserAgent,
		ListenAddress:      defaultListenAddress,
		DataPath:           defaultDataPath,
		AdminPassword:      defaultAdminPassword,
		EncryptionKey:      base64.RawURLEncoding.EncodeToString(key),
		PlatformBaseURL:    defaultPlatformBaseURL,
		CodingPlanAPIURL:   defaultCodingPlanAPIURL,
		GatewayURL:         defaultGatewayURL,
		RequestTimeoutSecs: 600,
		RequestRetryCount:  3,
		RetryStatusCodes:   "429",
	}
	if _, err := normalizeConfig(&config); err != nil {
		return Config{}, err
	}
	return config, nil
}

func normalizeConfig(config *Config) (bool, error) {
	changed := false
	setString := func(target *string, value string) {
		if strings.TrimSpace(*target) == "" {
			*target = value
			changed = true
		}
	}
	setString(&config.UserAgent, defaultUserAgent)
	setString(&config.ListenAddress, defaultListenAddress)
	setString(&config.DataPath, defaultDataPath)
	if strings.EqualFold(filepath.Ext(config.DataPath), ".json") {
		config.DataPath = strings.TrimSuffix(config.DataPath, filepath.Ext(config.DataPath)) + ".db"
		changed = true
	}
	setString(&config.AdminPassword, defaultAdminPassword)
	setString(&config.PlatformBaseURL, defaultPlatformBaseURL)
	setString(&config.CodingPlanAPIURL, defaultCodingPlanAPIURL)
	setString(&config.GatewayURL, defaultGatewayURL)
	if strings.TrimSpace(config.SystemPrompt) == "" {
		config.SystemPrompt = defaultSystemPrompt
		changed = true
	}
	normalizedPassword, passwordChanged, err := normalizeAdminPassword(config.AdminPassword)
	if err != nil {
		return false, err
	}
	config.AdminPassword = normalizedPassword
	changed = changed || passwordChanged
	if config.RequestTimeoutSecs == 0 {
		config.RequestTimeoutSecs = 600
		changed = true
	}
	if config.AuditRetentionDays == 0 {
		config.AuditRetentionDays = defaultAuditRetention
		changed = true
	}
	if config.AuditDetailRetentionDays == 0 {
		config.AuditDetailRetentionDays = defaultAuditRetention
		changed = true
	}
	retryStatuses, err := parseRetryStatusCodes(config.RetryStatusCodes)
	if err != nil {
		return false, err
	}
	if normalized := retryStatuses.String(); normalized != config.RetryStatusCodes {
		config.RetryStatusCodes = normalized
		changed = true
	}
	if strings.TrimSpace(config.EncryptionKey) == "" {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return false, fmt.Errorf("generate encryption key: %w", err)
		}
		config.EncryptionKey = base64.RawURLEncoding.EncodeToString(key)
		changed = true
	}
	return changed, nil
}

func (m *ConfigManager) Start(interval time.Duration) {
	if interval <= 0 {
		interval = time.Second
	}
	m.startOnce.Do(func() {
		go func() {
			defer close(m.done)
			ticker := time.NewTicker(interval)
			defer ticker.Stop()

			lastError := ""
			for {
				select {
				case <-ticker.C:
					changed, err := m.Reload()
					if err != nil {
						if message := err.Error(); message != lastError {
							log.Printf("reload config: %v", err)
							lastError = message
						}
						continue
					}
					lastError = ""
					if changed {
						log.Printf("configuration reloaded from %s", m.path)
					}
				case <-m.stop:
					return
				}
			}
		}()
	})
}

func (m *ConfigManager) Close() {
	m.stopOnce.Do(func() { close(m.stop) })
	select {
	case <-m.done:
	case <-time.After(2 * time.Second):
	}
}

func (m *ConfigManager) Snapshot() ConfigSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return ConfigSnapshot{Config: m.config, LoadedAt: m.loadedAt}
}

func (m *ConfigManager) Reload() (bool, error) {
	data, err := os.ReadFile(m.path)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", m.path, err)
	}

	hash := sha256.Sum256(data)
	m.mu.RLock()
	unchanged := hash == m.fileHash
	m.mu.RUnlock()
	if unchanged {
		return false, nil
	}

	config, normalized, err := parseConfig(data)
	if err != nil {
		return false, err
	}
	if normalized {
		if err := m.store(config); err != nil {
			return false, err
		}
		return true, nil
	}

	m.mu.Lock()
	m.config = config
	m.fileHash = hash
	m.loadedAt = time.Now()
	m.mu.Unlock()
	return true, nil
}

func (m *ConfigManager) Update(config Config) error {
	if strings.TrimSpace(config.UserAgent) == "" {
		return errors.New("user_agent must not be empty")
	}
	if _, err := normalizeConfig(&config); err != nil {
		return err
	}
	if err := validateConfig(config); err != nil {
		return err
	}
	return m.store(config)
}

func (m *ConfigManager) store(config Config) error {
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	data = append(data, '\n')

	directory := filepath.Dir(m.path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}

	temporary, err := os.CreateTemp(directory, ".atom2api-config-*")
	if err != nil {
		return fmt.Errorf("create temporary config: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)

	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("secure temporary config: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return fmt.Errorf("write temporary config: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync temporary config: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary config: %w", err)
	}
	if err := replaceFile(temporaryPath, m.path); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}

	m.mu.Lock()
	m.config = config
	m.fileHash = sha256.Sum256(data)
	m.loadedAt = time.Now()
	m.mu.Unlock()
	return nil
}

func parseConfig(data []byte) (Config, bool, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()

	var config Config
	if err := decoder.Decode(&config); err != nil {
		return Config{}, false, fmt.Errorf("decode config: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return Config{}, false, fmt.Errorf("decode config: %w", err)
	}
	if strings.TrimSpace(config.UserAgent) == "" {
		return Config{}, false, errors.New("user_agent must not be empty")
	}
	changed, err := normalizeConfig(&config)
	if err != nil {
		return Config{}, false, err
	}
	if err := validateConfig(config); err != nil {
		return Config{}, false, err
	}
	return config, changed, nil
}

func validateConfig(config Config) error {
	if strings.TrimSpace(config.UserAgent) == "" {
		return errors.New("user_agent must not be empty")
	}
	if len(config.UserAgent) > 512 {
		return errors.New("user_agent must not exceed 512 bytes")
	}
	if len([]byte(config.SystemPrompt)) > maxSystemPromptBytes {
		return fmt.Errorf("system_prompt must not exceed %d bytes", maxSystemPromptBytes)
	}
	for _, character := range config.SystemPrompt {
		if (character < 0x20 && character != '\t' && character != '\n' && character != '\r') || character == 0x7f {
			return errors.New("system_prompt contains a control character")
		}
	}
	for _, character := range config.UserAgent {
		if character < 0x20 || character == 0x7f {
			return errors.New("user_agent contains a control character")
		}
	}
	if !adminPasswordIsHash(config.AdminPassword) {
		return errors.New("admin_password must be a bcrypt hash")
	}
	if config.RequestTimeoutSecs < 5 || config.RequestTimeoutSecs > 600 {
		return errors.New("request_timeout_seconds must be between 5 and 600")
	}
	if config.RequestRetryCount < 0 || config.RequestRetryCount > 10 {
		return errors.New("request_retry_count must be between 0 and 10")
	}
	retryStatuses, err := parseRetryStatusCodes(config.RetryStatusCodes)
	if err != nil {
		return err
	}
	if config.RequestRetryCount > 0 && retryStatuses.String() == "" {
		return errors.New("request_retry_status_codes must not be empty when request retries are enabled")
	}
	if config.AuditRetentionDays < 1 || config.AuditRetentionDays > maxAuditRetention {
		return fmt.Errorf("audit_retention_days must be between 1 and %d", maxAuditRetention)
	}
	if config.AuditDetailRetentionDays < 1 || config.AuditDetailRetentionDays > maxAuditRetention {
		return fmt.Errorf("audit_detail_retention_days must be between 1 and %d", maxAuditRetention)
	}
	if _, err := encryptionKey(config.EncryptionKey); err != nil {
		return err
	}
	for name, value := range map[string]string{
		"platform_base_url":  config.PlatformBaseURL,
		"codingplan_api_url": config.CodingPlanAPIURL,
		"gateway_url":        config.GatewayURL,
	} {
		parsed, err := url.ParseRequestURI(value)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return fmt.Errorf("%s must be an absolute URL", name)
		}
	}
	if config.SignerURL != "" {
		parsed, err := url.ParseRequestURI(config.SignerURL)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return errors.New("signer_url must be an absolute URL")
		}
		host := parsed.Hostname()
		ip := net.ParseIP(host)
		loopback := strings.EqualFold(host, "localhost") || (ip != nil && ip.IsLoopback())
		if parsed.Scheme != "https" && !(parsed.Scheme == "http" && loopback) {
			return errors.New("signer_url must use HTTPS unless it targets a loopback address")
		}
	}
	return nil
}

func encryptionKey(encoded string) ([]byte, error) {
	key, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(key) != 32 {
		return nil, errors.New("encryption_key must be a base64url-encoded 32-byte key")
	}
	return key, nil
}

func replaceFile(source, destination string) error {
	if err := os.Rename(source, destination); err == nil {
		return nil
	}
	if err := os.Remove(destination); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(source, destination)
}
