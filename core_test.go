package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestStore(t *testing.T) (*ConfigManager, *Store) {
	t.Helper()
	directory := t.TempDir()
	config, err := NewConfigManager(filepath.Join(directory, "config.json"))
	if err != nil {
		t.Fatalf("NewConfigManager: %v", err)
	}
	store, err := NewStore(filepath.Join(directory, "state.json"), config)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return config, store
}

func storedUsageData(t *testing.T, store *Store) []byte {
	t.Helper()
	rows, err := store.db.Query(`SELECT data FROM atom2api_usage_records ORDER BY seq`)
	if err != nil {
		t.Fatalf("query stored usage: %v", err)
	}
	defer rows.Close()
	var result []byte
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			t.Fatalf("scan stored usage: %v", err)
		}
		result = append(result, data...)
		result = append(result, '\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read stored usage: %v", err)
	}
	return result
}

func addTestAccount(t *testing.T, store *Store, upstreamURL string) AccountView {
	t.Helper()
	view, err := store.UpsertAccount(Account{
		Name: "Test Account", Status: "active", Enabled: true,
		User:        UserInfo{ID: "user-1", Username: "tester", Name: "Tester"},
		Credentials: OAuthCredentials{TokenType: "Bearer", CreatedAt: time.Now().UTC()},
		Plan:        CodingPlanStatus{Plan: &PlanInfo{PlanName: "CodingPlan Pro"}},
		Models: []CodingPlanModel{{
			DisplayModelName: "upstream-model", BaseURL: upstreamURL + "/v1",
			ProviderType: "openai", ContextWindow: 128000, PlanAvailable: true,
		}},
	}, "oauth-access-secret", "oauth-refresh-secret")
	if err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}
	return view
}

func TestAccountViewSerializesNilModelsAsEmptyArray(t *testing.T) {
	view := (Account{}).View()
	if view.Models == nil {
		t.Fatal("account view models is nil")
	}

	data, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("marshal account view: %v", err)
	}
	var payload struct {
		Models json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("decode account view: %v", err)
	}
	if string(payload.Models) != "[]" {
		t.Fatalf("models JSON = %s, want []", payload.Models)
	}
}

func TestStoreEncryptsOAuthCredentialsAndHashesAPIKeys(t *testing.T) {
	_, store := newTestStore(t)
	view := addTestAccount(t, store, "https://example.com")
	key, secret, err := store.CreateAPIKey("production", nil, nil)
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	data, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	content := string(data)
	for _, forbidden := range []string{"oauth-access-secret", "oauth-refresh-secret", secret} {
		if strings.Contains(content, forbidden) {
			t.Fatalf("state file contains plaintext secret %q", forbidden)
		}
	}
	if _, ok := store.AuthenticateAPIKey(secret); !ok {
		t.Fatal("generated API key did not authenticate")
	}
	if _, ok := store.AuthenticateAPIKey(secret + "x"); ok {
		t.Fatal("modified API key authenticated")
	}
	account, access, refresh, err := store.Account(view.ID)
	if err != nil || account.ID != view.ID || access != "oauth-access-secret" || refresh != "oauth-refresh-secret" {
		t.Fatalf("decrypted account = (%q, %q, %q, %v)", account.ID, access, refresh, err)
	}
	if key.Prefix == "" || strings.Contains(key.Prefix, secret) {
		t.Fatalf("unexpected key prefix %q", key.Prefix)
	}
}

func TestCodingPlanClaimCascadeAndSync(t *testing.T) {
	config, store := newTestStore(t)
	var mu sync.Mutex
	claimTiers := []string{}
	claimed := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer oauth-access-secret" {
			t.Errorf("Authorization = %q", got)
		}
		switch r.URL.Path {
		case "/coding-plan/status-v2":
			mu.Lock()
			isClaimed := claimed
			mu.Unlock()
			if !isClaimed {
				_, _ = w.Write([]byte(`{"codingplan_free":null,"rate_limit_windows":[]}`))
				return
			}
			_, _ = w.Write([]byte(`{"codingplan_free":{"plan_name":"CodingPlan Pro","status":1,"remaining_days":7,"total_days":7},"rate_limit_windows":[{"show_enable":1,"window_size_seconds":18000,"call_limit":100,"calls_used":12,"usage_percent":12}]}`))
		case "/coding-plan/claim-v2":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			claimTiers = append(claimTiers, body["plan_type"])
			if body["plan_type"] == "Pro" {
				claimed = true
			}
			mu.Unlock()
			if body["plan_type"] == "Pro" {
				_, _ = w.Write([]byte(`{"success":true,"duplicate":false,"plan_name":"CodingPlan Pro"}`))
			} else {
				_, _ = w.Write([]byte(`{"success":false,"duplicate":false,"message":"not eligible"}`))
			}
		case "/coding-plan/models-v2":
			if got := r.URL.Query().Get("plan_type"); got != "Pro" {
				t.Errorf("plan_type = %q", got)
			}
			_, _ = w.Write([]byte(`[{"display_model_name":"model-a","base_url":"https://example.com/v1","type":"openai","context_window":128000,"plan_available":true},{"display_model_name":"locked","plan_available":false}]`))
		case "/coding-plan/usage":
			_, _ = w.Write([]byte(`{"days":60,"total_tokens":1234,"total_counts":9,"rows":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	updated := config.Snapshot().Config
	updated.CodingPlanAPIURL = server.URL
	if err := config.Update(updated); err != nil {
		t.Fatalf("update config: %v", err)
	}
	account := addTestAccount(t, store, "https://example.com")
	_, err := store.UpdateAccount(account.ID, func(account *Account) error {
		account.Plan = CodingPlanStatus{}
		account.Models = nil
		return nil
	})
	if err != nil {
		t.Fatalf("clear account: %v", err)
	}
	client := NewCodingPlanClient(config, store)
	outcome, err := client.ClaimAndSyncDetailed(context.Background(), account.ID)
	if err != nil {
		t.Fatalf("ClaimAndSyncDetailed: %v", err)
	}
	view := outcome.Account
	if got := strings.Join(claimTiers, ","); got != "Max,Pro" {
		t.Fatalf("claim tiers = %q", got)
	}
	if len(outcome.Attempts) != 2 || outcome.Attempts[0].PlanType != "Max" || outcome.Attempts[0].Message != "not eligible" || outcome.Attempts[0].HTTPStatus != http.StatusOK {
		t.Fatalf("claim attempts = %#v", outcome.Attempts)
	}
	if !outcome.Attempts[1].Success || outcome.Attempts[1].PlanType != "Pro" || !strings.Contains(outcome.Attempts[1].Response, `"success":true`) {
		t.Fatalf("successful claim attempt = %#v", outcome.Attempts[1])
	}
	if view.Plan.Plan == nil || view.Plan.Plan.PlanName != "CodingPlan Pro" {
		t.Fatalf("plan = %#v", view.Plan.Plan)
	}
	if len(view.Models) != 1 || view.Models[0].DisplayModelName != "model-a" {
		t.Fatalf("models = %#v", view.Models)
	}
	if view.ProviderUsage == nil || view.ProviderUsage.TotalTokens != 1234 {
		t.Fatalf("provider usage = %#v", view.ProviderUsage)
	}
}

func TestProxyNonStreamingRewritesModelAndRecordsUsage(t *testing.T) {
	var upstreamModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		var request map[string]any
		_ = json.NewDecoder(r.Body).Decode(&request)
		upstreamModel, _ = request["model"].(string)
		time.Sleep(10 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","model":"upstream-model","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18,"prompt_tokens_details":{"cached_tokens":3}}}`))
	}))
	defer upstream.Close()

	config, store := newTestStore(t)
	account := addTestAccount(t, store, upstream.URL)
	if err := store.SetModelSetting(ModelSetting{Upstream: "upstream-model", Alias: "gpt-test", Enabled: true}); err != nil {
		t.Fatalf("SetModelSetting: %v", err)
	}
	_, secret, err := store.CreateAPIKey("test", []string{"gpt-test"}, nil)
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	router := NewModelRouter(store)
	proxy := NewProxy(config, store, router, nil)
	api := NewAPI(store, nil, nil, router, proxy)
	handler := api.RequireAPIKey(proxy.HandleRequest)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-test","messages":[{"role":"user","content":"hello"}]}`))
	request.Header.Set("Authorization", "Bearer "+secret)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if upstreamModel != "upstream-model" {
		t.Fatalf("upstream model = %q", upstreamModel)
	}
	var body map[string]any
	_ = json.Unmarshal(response.Body.Bytes(), &body)
	if body["model"] != "gpt-test" {
		t.Fatalf("response model = %#v", body["model"])
	}
	records := store.UsageRecords()
	if len(records) != 1 || records[0].InputTokens != 11 || records[0].OutputTokens != 7 || records[0].CachedTokens != 3 || records[0].AccountID != account.ID {
		t.Fatalf("usage records = %#v", records)
	}
	if records[0].LatencyMS < 5 || records[0].FirstTokenLatencyMS != 0 || records[0].CompletionLatencyMS != 0 {
		t.Fatalf("non-stream timing = %#v", records[0])
	}
	if records[0].Method != http.MethodPost || records[0].RequestBody != "" || records[0].ResponseBody != "" || len(records[0].RequestHeaders) != 0 || len(records[0].ResponseHeaders) != 0 {
		t.Fatalf("audit debug details were recorded while disabled: %#v", records[0])
	}
	data := storedUsageData(t, store)
	if !bytes.Contains(data, []byte(`"input_tokens":11`)) {
		t.Fatalf("stored usage = %s", data)
	}
}

func TestProxyStreamingForwardsSSEAndRecordsFinalUsage(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		_ = json.NewDecoder(r.Body).Decode(&request)
		options, _ := request["stream_options"].(map[string]any)
		if include, _ := options["include_usage"].(bool); !include {
			t.Error("stream_options.include_usage was not injected")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		time.Sleep(5 * time.Millisecond)
		_, _ = w.Write([]byte(`data: {"id":"chatcmpl-2","model":"upstream-model","choices":[{"delta":{"role":"assistant"}}]}` + "\n\n"))
		flusher.Flush()
		time.Sleep(10 * time.Millisecond)
		_, _ = w.Write([]byte(`data: {"id":"chatcmpl-2","model":"upstream-model","choices":[{"delta":{"content":"hi"}}]}` + "\n\n"))
		flusher.Flush()
		time.Sleep(15 * time.Millisecond)
		for _, line := range []string{
			`data: {"id":"chatcmpl-2","model":"upstream-model","choices":[],"usage":{"prompt_tokens":20,"completion_tokens":4,"total_tokens":24}}` + "\n\n",
			"data: [DONE]\n\n",
		} {
			_, _ = w.Write([]byte(line))
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	config, store := newTestStore(t)
	addTestAccount(t, store, upstream.URL)
	_ = store.SetModelSetting(ModelSetting{Upstream: "upstream-model", Alias: "gpt-stream", Enabled: true})
	_, secret, _ := store.CreateAPIKey("stream", nil, nil)
	router := NewModelRouter(store)
	proxy := NewProxy(config, store, router, nil)
	api := NewAPI(store, nil, nil, router, proxy)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-stream","stream":true,"messages":[]}`))
	request.Header.Set("Authorization", "Bearer "+secret)
	response := httptest.NewRecorder()
	api.RequireAPIKey(proxy.HandleRequest).ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"model":"gpt-stream"`) || !strings.Contains(response.Body.String(), "data: [DONE]") {
		t.Fatalf("stream response (%d) = %s", response.Code, response.Body.String())
	}
	records := store.UsageRecords()
	if len(records) != 1 || !records[0].Streaming || records[0].InputTokens != 20 || records[0].OutputTokens != 4 {
		t.Fatalf("stream usage = %#v", records)
	}
	if records[0].FirstTokenLatencyMS < 10 || records[0].CompletionLatencyMS < 10 || records[0].LatencyMS < records[0].FirstTokenLatencyMS+records[0].CompletionLatencyMS {
		t.Fatalf("stream timing = %#v", records[0])
	}
	if records[0].Method != http.MethodPost || records[0].RequestBody != "" || records[0].ResponseBody != "" {
		t.Fatalf("stream audit debug details were recorded while disabled: %#v", records[0])
	}
	if len(storedUsageData(t, store)) == 0 {
		t.Fatal("stored usage is empty")
	}
}

func TestStreamChunkHasOutputIgnoresMetadataEvents(t *testing.T) {
	tests := []struct {
		name string
		data string
		want bool
	}{
		{name: "responses created", data: `{"type":"response.created","response":{"id":"resp_1"}}`, want: false},
		{name: "chat role", data: `{"choices":[{"delta":{"role":"assistant"}}]}`, want: false},
		{name: "chat text", data: `{"choices":[{"delta":{"content":"hello"}}]}`, want: true},
		{name: "chat tool", data: `{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]}}]}`, want: true},
		{name: "responses text", data: `{"type":"response.output_text.delta","delta":"hello"}`, want: true},
		{name: "responses tool", data: `{"type":"response.function_call_arguments.delta","delta":"{}"}`, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := streamChunkHasOutput([]byte(test.data)); got != test.want {
				t.Fatalf("streamChunkHasOutput() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestAuditHandlersKeepBodiesOutOfListAndReturnDetail(t *testing.T) {
	_, store := newTestStore(t)
	now := time.Now().UTC()
	record := UsageRecord{
		ID: "req_audit_detail", Timestamp: now, Path: "/v1/chat/completions", Model: "gpt-audit",
		Status: http.StatusOK, LatencyMS: 42, FirstTokenLatencyMS: 12, CompletionLatencyMS: 30,
		InputTokens: 5, OutputTokens: 7, Streaming: true,
		RequestBody:     `{"model":"gpt-audit","messages":[]}`,
		ResponseBody:    `{"id":"chatcmpl-audit","choices":[]}`,
		RequestHeaders:  map[string][]string{"Content-Type": {"application/json"}},
		ResponseHeaders: map[string][]string{"X-Request-Id": {"upstream-request"}},
	}
	if err := store.RecordUsage(record); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	if err := store.RecordUsage(UsageRecord{
		ID: "req_audit_error", Timestamp: now.Add(time.Second), Method: http.MethodPost,
		Path: "/v1/responses", Model: "gpt-error", Status: http.StatusBadGateway,
		RequestBody: `{"model":"gpt-error"}`, ResponseBody: `{"error":"upstream failed"}`,
	}); err != nil {
		t.Fatalf("RecordUsage error entry: %v", err)
	}

	api := &API{store: store}
	listRequest := httptest.NewRequest(http.MethodGet, "/api/audit?status=success&page=1&page_size=20", nil)
	listResponse := httptest.NewRecorder()
	api.HandleAudits(listResponse, listRequest)
	if listResponse.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", listResponse.Code, listResponse.Body.String())
	}
	if strings.Contains(listResponse.Body.String(), `"request_body":`) || strings.Contains(listResponse.Body.String(), `"response_body":`) ||
		strings.Contains(listResponse.Body.String(), `"request_headers":`) || strings.Contains(listResponse.Body.String(), `"response_headers":`) ||
		strings.Contains(listResponse.Body.String(), "chatcmpl-audit") || strings.Contains(listResponse.Body.String(), "upstream-request") {
		t.Fatalf("audit list contains full details: %s", listResponse.Body.String())
	}
	var list auditListResponse
	if err := json.Unmarshal(listResponse.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if list.Total != 1 || len(list.Items) != 1 || list.Items[0].ID != record.ID || list.Items[0].Method != http.MethodPost ||
		list.Items[0].FirstTokenLatencyMS != 12 || list.Items[0].CompletionLatencyMS != 30 || !list.Items[0].HasRequestBody || !list.Items[0].HasResponseBody {
		t.Fatalf("audit list = %#v", list)
	}

	detailRequest := httptest.NewRequest(http.MethodGet, "/api/audit/"+record.ID, nil)
	detailRequest.SetPathValue("id", record.ID)
	detailResponse := httptest.NewRecorder()
	api.HandleAuditDetail(detailResponse, detailRequest)
	if detailResponse.Code != http.StatusOK {
		t.Fatalf("detail status = %d, body = %s", detailResponse.Code, detailResponse.Body.String())
	}
	var detail auditDetailResponse
	if err := json.Unmarshal(detailResponse.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if detail.Method != http.MethodPost || detail.FirstTokenLatencyMS != 12 || detail.CompletionLatencyMS != 30 ||
		detail.RequestBody != record.RequestBody || detail.ResponseBody != record.ResponseBody ||
		detail.RequestHeaders["Content-Type"][0] != "application/json" || detail.ResponseHeaders["X-Request-Id"][0] != "upstream-request" {
		t.Fatalf("audit detail = %#v", detail)
	}
}

func TestAdminLoginUsesServerSessionCookie(t *testing.T) {
	config, _ := newTestStore(t)
	auth := NewAdminAuth(config)
	request := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"password":"atom2api"}`))
	response := httptest.NewRecorder()
	auth.HandleLogin(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	result := response.Result()
	defer result.Body.Close()
	cookies := result.Cookies()
	var sessionCookie, rememberCookie *http.Cookie
	for _, cookie := range cookies {
		switch cookie.Name {
		case adminCookieName:
			sessionCookie = cookie
		case adminRememberCookie:
			rememberCookie = cookie
		}
	}
	if sessionCookie == nil || !sessionCookie.HttpOnly || sessionCookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("session cookie = %#v", sessionCookie)
	}
	if rememberCookie == nil || rememberCookie.Value == "" {
		t.Fatalf("remember cookie missing, cookies = %#v", cookies)
	}
	protectedRequest := httptest.NewRequest(http.MethodGet, "/api/accounts", nil)
	protectedRequest.AddCookie(sessionCookie)
	protectedResponse := httptest.NewRecorder()
	auth.Require(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })).ServeHTTP(protectedResponse, protectedRequest)
	if protectedResponse.Code != http.StatusNoContent {
		t.Fatalf("protected status = %d", protectedResponse.Code)
	}
}

func TestAtomGitSigningMatchesIndependentFixture(t *testing.T) {
	nonce := make([]byte, 16)
	for i := range nonce {
		nonce[i] = byte(i)
	}
	headers, err := signAtomGitRequest(
		http.MethodPost,
		"/v1/chat/completions",
		[]byte(`{"model":"deepseek-v4-flash"}`),
		"token-fixture",
		"user-fixture",
		"5.0.2",
		1767225600,
		nonce,
		"",
	)
	if err != nil {
		t.Fatalf("signAtomGitRequest: %v", err)
	}
	if got, want := headers["X-AtomCode-Sig"], "v1:c5bafa716c0fdc31a6e2738412d0b21a9d9e9d1ca09cc44eb17949a1b4fa4bdd"; got != want {
		t.Fatalf("signature = %q, want %q", got, want)
	}
	if headers["X-AtomCode-Nonce"] != "000102030405060708090a0b0c0d0e0f" || headers["X-AtomCode-Ts"] != "1767225600" || headers["X-AtomCode-Ver"] != "5.0.2" {
		t.Fatalf("headers = %#v", headers)
	}
}

func TestExpiredQuotaWindowReturnsToRoutingPool(t *testing.T) {
	now := time.Now().UTC()
	status := CodingPlanStatus{RateLimitWindows: []RateLimitWindow{{
		ShowEnable: 1, QuotaExhausted: true, SecondsUntilReset: 3600,
	}}}
	if !quotaExhausted(status, &now) {
		t.Fatal("fresh exhausted window was treated as available")
	}
	twoHoursAgo := now.Add(-2 * time.Hour)
	if quotaExhausted(status, &twoHoursAgo) {
		t.Fatal("expired exhausted window remained blocked")
	}
}

func TestConfigRejectsRemotePlaintextSigner(t *testing.T) {
	config, err := defaultConfig()
	if err != nil {
		t.Fatal(err)
	}
	config.SignerURL = "http://signer.example.com/v1/sign"
	if err := validateConfig(config); err == nil {
		t.Fatal("plaintext remote signer URL was accepted")
	}
	config.SignerURL = "http://127.0.0.1:9457/v1/sign"
	if err := validateConfig(config); err != nil {
		t.Fatalf("loopback signer URL was rejected: %v", err)
	}
}
