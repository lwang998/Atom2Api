package main

import (
	"testing"
	"time"
)

func addRoutingAccount(t *testing.T, store *Store, id, planName string, models ...string) AccountView {
	t.Helper()
	available := make([]CodingPlanModel, 0, len(models))
	for _, model := range models {
		available = append(available, CodingPlanModel{DisplayModelName: model, BaseURL: "https://example.com/v1", ProviderType: "openai", PlanAvailable: true})
	}
	view, err := store.UpsertAccount(Account{
		ID: id, Name: id, Status: "active", Enabled: true,
		User:        UserInfo{ID: id, Username: id},
		Credentials: OAuthCredentials{TokenType: "Bearer", CreatedAt: time.Now().UTC()},
		Plan:        routingPlanStatus(planName), Models: available,
	}, "access-"+id, "refresh-"+id)
	if err != nil {
		t.Fatalf("UpsertAccount(%s): %v", id, err)
	}
	return view
}

func routingPlanStatus(planName string) CodingPlanStatus {
	return CodingPlanStatus{Plan: &PlanInfo{PlanName: planName}}
}

func TestFillRoutingBindsEachKeyPerModelAndPrioritizesPlanTier(t *testing.T) {
	_, store := newTestStore(t)
	addRoutingAccount(t, store, "lite-1", "CodingPlan Lite", "deepseek-v3", "GLM-5.2")
	addRoutingAccount(t, store, "lite-2", "CodingPlan Lite", "deepseek-v3", "GLM-5.2")
	addRoutingAccount(t, store, "pro-1", "CodingPlan Pro", "deepseek-v3", "GLM-5.2")
	if err := store.SetModelSetting(ModelSetting{Upstream: "deepseek-v3", Alias: "deepseek", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetModelSetting(ModelSetting{Upstream: "GLM-5.2", Alias: "glm", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	router := NewModelRouter(store)
	_, secret1, err := store.CreateAPIKey("key-1", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, secret2, err := store.CreateAPIKey("key-2", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	key1, ok := store.AuthenticateAPIKey(secret1)
	if !ok {
		t.Fatal("key1 authentication failed")
	}
	key2, ok := store.AuthenticateAPIKey(secret2)
	if !ok {
		t.Fatal("key2 authentication failed")
	}

	deepSeek1, err := router.Resolve("deepseek", key1)
	if err != nil {
		t.Fatal(err)
	}
	deepSeek1Again, err := router.Resolve("deepseek", key1)
	if err != nil {
		t.Fatal(err)
	}
	deepSeek2, err := router.Resolve("deepseek", key2)
	if err != nil {
		t.Fatal(err)
	}
	glm1, err := router.Resolve("glm", key1)
	if err != nil {
		t.Fatal(err)
	}
	glm2, err := router.Resolve("glm", key2)
	if err != nil {
		t.Fatal(err)
	}

	if deepSeek1.Account.ID != "lite-1" || deepSeek1Again.Account.ID != deepSeek1.Account.ID {
		t.Fatalf("key1 DeepSeek binding = %q then %q", deepSeek1.Account.ID, deepSeek1Again.Account.ID)
	}
	if deepSeek2.Account.ID != "lite-2" {
		t.Fatalf("key2 DeepSeek binding = %q, want lite-2", deepSeek2.Account.ID)
	}
	if glm1.Account.ID != "pro-1" || glm2.Account.ID != "pro-1" {
		t.Fatalf("GLM bindings = %q, %q, want shared pro-1", glm1.Account.ID, glm2.Account.ID)
	}
	reloadedKey1, ok := store.AuthenticateAPIKey(secret1)
	if !ok {
		t.Fatal("reloaded key1 authentication failed")
	}
	reloadedDeepSeek, err := NewModelRouter(store).Resolve("deepseek", reloadedKey1)
	if err != nil || reloadedDeepSeek.Account.ID != deepSeek1.Account.ID {
		t.Fatalf("persisted DeepSeek binding = %q, want %q (err=%v)", reloadedDeepSeek.Account.ID, deepSeek1.Account.ID, err)
	}

	views := store.APIKeys()
	if len(views) != 2 {
		t.Fatalf("API key views = %#v", views)
	}
	for _, view := range views {
		if len(view.RouteBindings) != 2 {
			t.Fatalf("%s persisted bindings = %#v", view.ID, view.RouteBindings)
		}
	}
}

func TestDeepSeekFallsBackFromExhaustedLiteToPro(t *testing.T) {
	_, store := newTestStore(t)
	now := time.Now().UTC()
	lite := addRoutingAccount(t, store, "lite-exhausted", "CodingPlan Lite", "deepseek-v3")
	_, err := store.UpdateAccount(lite.ID, func(account *Account) error {
		account.LastSyncAt = &now
		account.Plan.RateLimitWindows = []RateLimitWindow{{ShowEnable: 1, WindowSizeSeconds: 18000, WindowHours: 5, CallLimit: 10, CallsUsed: 10, QuotaExhausted: true, SecondsUntilReset: 3600}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	addRoutingAccount(t, store, "pro-fallback", "CodingPlan Pro", "deepseek-v3")
	if err := store.SetModelSetting(ModelSetting{Upstream: "deepseek-v3", Alias: "deepseek", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	route, err := NewModelRouter(store).Resolve("deepseek", APIKey{ID: "key-fallback", RouteStrategy: RouteStrategyFill})
	if err != nil {
		t.Fatal(err)
	}
	if route.Account.ID != "pro-fallback" {
		t.Fatalf("fallback account = %q, want pro-fallback", route.Account.ID)
	}
}

func TestRoundRobinRoutingDoesNotReuseFillBinding(t *testing.T) {
	_, store := newTestStore(t)
	addRoutingAccount(t, store, "account-a", "CodingPlan Pro", "model-a")
	addRoutingAccount(t, store, "account-b", "CodingPlan Pro", "model-a")
	if err := store.SetModelSetting(ModelSetting{Upstream: "model-a", Alias: "model-a", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	router := NewModelRouter(store)
	key := APIKey{ID: "key-random", RouteStrategy: RouteStrategyRoundRobin}
	seen := map[string]bool{}
	for i := 0; i < 32; i++ {
		route, err := router.Resolve("model-a", key)
		if err != nil {
			t.Fatal(err)
		}
		seen[route.Account.ID] = true
	}
	if len(seen) != 2 {
		t.Fatalf("round-robin/random routes only used %#v", seen)
	}
}

func TestResolveIgnoresContextSuffix(t *testing.T) {
	_, store := newTestStore(t)
	addRoutingAccount(t, store, "account-flash", "CodingPlan Lite", "glm5.3-flash")
	router := NewModelRouter(store)
	_, secret, err := store.CreateAPIKey("key-1", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	key, ok := store.AuthenticateAPIKey(secret)
	if !ok {
		t.Fatal("key authentication failed")
	}
	for _, requested := range []string{"glm5.3-flash", "glm5.3-flash[1m]", "glm5.3-flash[1M]"} {
		route, err := router.Resolve(requested, key)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", requested, err)
		}
		if route.Upstream != "glm5.3-flash" {
			t.Fatalf("Resolve(%q) upstream = %q, want glm5.3-flash", requested, route.Upstream)
		}
	}
	if _, err := router.Resolve("[1m]", key); err == nil {
		t.Fatal("Resolve(\"[1m]\") should fail, not resolve to an empty model")
	}
}
