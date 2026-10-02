package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

//go:embed all:frontend/dist
var frontendAssets embed.FS

var version = "dev"

type healthResponse struct {
	Status  string `json:"status"`
	Service string `json:"service"`
	Version string `json:"version"`
}

func main() {
	if len(os.Args) == 2 && (os.Args[1] == "--version" || os.Args[1] == "-version") {
		fmt.Println(version)
		return
	}

	dist, err := fs.Sub(frontendAssets, "frontend/dist")
	if err != nil {
		log.Fatal(err)
	}

	configPath, homeDefault := resolveConfigPath(os.Getenv("ATOM2API_CONFIG"))
	config, err := NewConfigManager(configPath)
	if err != nil {
		log.Fatal(err)
	}
	if homeDefault {
		hideFolder(filepath.Dir(configPath))
	}
	config.Start(time.Second)
	defer config.Close()
	snapshot := config.Snapshot()

	dataPath := resolveDataPath(snapshot.DataPath, configPath)
	if dir := filepath.Dir(dataPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Fatal(err)
		}
	}
	store, err := NewStore(dataPath, config)
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()
	codingPlan := NewCodingPlanClient(config, store)
	oauth := NewOAuthManager(config, store, codingPlan)
	codingPlan.SetOAuthManager(oauth)
	quotaRefresh := NewQuotaRefreshService(store, codingPlan)
	quotaRefresh.Start()
	defer quotaRefresh.Close()
	models := NewModelRouter(store)
	proxy := NewProxy(config, store, models, oauth)
	api := NewAPI(store, oauth, codingPlan, models, proxy)
	oauth.SetPlanClaimService(api.planClaims)
	releaseChecker := NewReleaseChecker(version)
	userAgentChecker := NewUserAgentChecker()
	if err := api.planClaims.Start(); err != nil {
		log.Fatal(err)
	}
	defer api.planClaims.Close()
	adminAuth := NewAdminAuth(config)

	adminMux := http.NewServeMux()
	adminMux.HandleFunc("GET /api/dashboard", api.HandleDashboard)
	adminMux.HandleFunc("GET /api/audit", api.HandleAudits)
	adminMux.HandleFunc("GET /api/audit/{id}", api.HandleAuditDetail)
	adminMux.HandleFunc("POST /api/audit/cleanup/records", api.HandleAuditRecordCleanup)
	adminMux.HandleFunc("POST /api/audit/cleanup/details", api.HandleAuditDetailCleanup)
	adminMux.HandleFunc("GET /api/accounts", api.HandleAccounts)
	adminMux.HandleFunc("GET /api/accounts/export", api.HandleAccountCredentialExport)
	adminMux.HandleFunc("GET /api/accounts/{id}/export", api.HandleAccountCredentialExport)
	adminMux.HandleFunc("POST /api/accounts/import", api.HandleAccountCredentialImport)
	adminMux.HandleFunc("DELETE /api/accounts/errors", api.HandleErrorAccountsDelete)
	adminMux.HandleFunc("PATCH /api/accounts/{id}", api.HandleAccountUpdate)
	adminMux.HandleFunc("DELETE /api/accounts/{id}", api.HandleAccountDelete)
	adminMux.HandleFunc("POST /api/accounts/{id}/sync", api.HandleAccountSync)
	adminMux.HandleFunc("POST /api/accounts/{id}/claim", api.HandleAccountClaim)
	adminMux.HandleFunc("POST /api/accounts/{id}/probe", api.HandleAccountProtocolProbe)
	adminMux.HandleFunc("GET /api/plan-claims", api.HandlePlanClaimLogs)
	adminMux.HandleFunc("POST /api/oauth/start", api.HandleOAuthStart)
	adminMux.HandleFunc("GET /api/oauth/{id}", api.HandleOAuthPoll)
	adminMux.HandleFunc("GET /api/keys", api.HandleKeys)
	adminMux.HandleFunc("POST /api/keys", api.HandleCreateKey)
	adminMux.HandleFunc("PATCH /api/keys/{id}", api.HandleUpdateKey)
	adminMux.HandleFunc("DELETE /api/keys/{id}", api.HandleDeleteKey)
	adminMux.HandleFunc("GET /api/models", api.HandleModels)
	adminMux.HandleFunc("POST /api/models", api.HandleCreateModel)
	adminMux.HandleFunc("PUT /api/models/settings", api.HandleModelSetting)
	adminMux.HandleFunc("DELETE /api/models/settings", api.HandleDeleteModel)
	adminMux.HandleFunc("GET /api/version", releaseChecker.HandleVersion)
	adminMux.HandleFunc("GET /api/settings", handleGetSettings(config))
	adminMux.HandleFunc("PUT /api/settings", handleUpdateSettings(config))
	adminMux.HandleFunc("POST /api/settings/user-agent/check", userAgentChecker.HandleCheck)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", handleHealth)
	mux.HandleFunc("POST /api/auth/login", adminAuth.HandleLogin)
	mux.HandleFunc("POST /api/auth/logout", adminAuth.HandleLogout)
	mux.HandleFunc("GET /api/auth/status", adminAuth.HandleStatus)
	mux.Handle("/api/", adminAuth.Require(adminMux))
	mux.HandleFunc("GET /v1/models", api.RequireAPIKey(proxy.HandleModels))
	mux.HandleFunc("POST /v1/messages", api.RequireAPIKey(proxy.HandleAnthropicMessages))
	mux.HandleFunc("POST /v1/messages/count_tokens", api.RequireAPIKey(proxy.HandleAnthropicCountTokens))
	for _, route := range []string{
		"POST /v1/chat/completions", "POST /v1/responses", "POST /v1/completions", "POST /v1/embeddings",
	} {
		mux.HandleFunc(route, api.RequireAPIKey(proxy.HandleRequest))
	}
	mux.Handle("/", spaHandler(dist))

	address := snapshot.ListenAddress
	if port := strings.TrimSpace(os.Getenv("PORT")); port != "" {
		address = ":" + strings.TrimPrefix(port, ":")
	}
	server := &http.Server{
		Addr: address, Handler: recoverMiddleware(securityHeaders(mux)),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second,
		IdleTimeout: 90 * time.Second, MaxHeaderBytes: 1 << 20,
	}

	if defaultAdminPasswordActive(snapshot.AdminPassword) {
		log.Printf("WARNING: default admin password is active; change it in Settings before exposing this service")
	}
	if snapshot.SignerURL == "" {
		log.Printf("AtomGit request signing: built-in atomcode-signing-v1")
	} else {
		log.Printf("AtomGit request signing: external signer %s", snapshot.SignerURL)
	}
	log.Printf("Atom2Api is running at http://localhost%s", displayAddress(address))

	if homeDefault && strings.TrimSpace(os.Getenv("ATOM2API_DESKTOP")) == "" {
		go openConsoleWhenReady(address)
	}

	serverErrors := make(chan error, 1)
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErrors <- err
		}
		close(serverErrors)
	}()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	select {
	case err := <-serverErrors:
		if err != nil {
			log.Fatal(err)
		}
	case <-signals:
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			log.Printf("shutdown: %v", err)
		}
	}
}

func displayAddress(address string) string {
	if strings.HasPrefix(address, ":") {
		return address
	}
	return "/ (listening on " + address + ")"
}

// resolveConfigPath decides where the configuration lives. An explicit
// ATOM2API_CONFIG wins; a legacy ./config.json next to the working directory
// keeps existing deployments untouched; otherwise the per-user hidden folder
// is the default so a double-clicked executable works with no setup.
func resolveConfigPath(env string) (path string, homeDefault bool) {
	if strings.TrimSpace(env) != "" {
		return env, false
	}
	if _, err := os.Stat(defaultConfigPath); err == nil {
		return defaultConfigPath, false
	}
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		return filepath.Join(home, ".atom2api", defaultConfigPath), true
	}
	return defaultConfigPath, false
}

// resolveDataPath anchors a relative data_path to the config file's directory
// so configs stored in ~/.atom2api keep their database beside them.
func resolveDataPath(dataPath, configPath string) string {
	if filepath.IsAbs(dataPath) {
		return dataPath
	}
	base := filepath.Dir(configPath)
	if base == "" || base == "." {
		return dataPath
	}
	return filepath.Join(base, dataPath)
}

// hideFolder marks the per-user data folder hidden on Windows; dot-prefixed
// names are already hidden on other platforms. Best-effort only.
func hideFolder(dir string) {
	if runtime.GOOS != "windows" || dir == "" || dir == "." {
		return
	}
	_ = exec.Command("attrib", "+h", filepath.Clean(dir)).Start()
}

// openConsoleWhenReady waits for the HTTP port then opens the console in the
// default browser, completing the double-click experience.
func openConsoleWhenReady(address string) {
	port := "8080"
	if _, parsed, err := net.SplitHostPort(address); err == nil && parsed != "" {
		port = parsed
	} else if strings.HasPrefix(address, ":") {
		port = strings.TrimPrefix(address, ":")
	}
	target := "http://127.0.0.1:" + port
	for attempt := 0; attempt < 100; attempt++ {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", port), time.Second)
		if err == nil {
			_ = conn.Close()
			var cmd *exec.Cmd
			switch runtime.GOOS {
			case "windows":
				cmd = exec.Command("cmd", "/c", "start", "", target)
			case "darwin":
				cmd = exec.Command("open", target)
			default:
				cmd = exec.Command("xdg-open", target)
			}
			_ = cmd.Start()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(healthResponse{
		Status: "ok", Service: "atom2api", Version: version,
	}); err != nil {
		log.Printf("encode health response: %v", err)
	}
}

func spaHandler(dist fs.FS) http.Handler {
	files := http.FileServer(http.FS(dist))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cleanPath := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if cleanPath != "." && cleanPath != "" {
			if info, err := fs.Stat(dist, cleanPath); err == nil && !info.IsDir() {
				if strings.HasPrefix(cleanPath, "assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				files.ServeHTTP(w, r)
				return
			}
		}
		w.Header().Set("Cache-Control", "no-cache")
		request := r.Clone(r.Context())
		request.URL.Path = "/"
		files.ServeHTTP(w, request)
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data: https:; style-src 'self' 'unsafe-inline'; script-src 'self'; connect-src 'self'")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		next.ServeHTTP(w, r)
	})
}

func recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				log.Printf("panic serving %s %s: %v", r.Method, r.URL.Path, recovered)
				writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
			}
		}()
		next.ServeHTTP(w, r)
	})
}
