package server

import (
	"context"
	"encoding/json"
	"io"
	"sync/atomic"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mojitex/fantom/internal/config"
)

type usageQueueCoreStub struct {
	CoreRouter
	records []json.RawMessage
}

func (s *usageQueueCoreStub) UsageQueue(_ context.Context, _ int) ([]json.RawMessage, error) {
	records := s.records
	s.records = nil
	return records, nil
}

func (s *usageQueueCoreStub) Close() {}

func TestCollectCoreUsageBatchPersistsUnifiedSafeUsageHistory(t *testing.T) {
	t.Setenv("FANTOM_DATA_DIR", t.TempDir())
	handler := New(&config.Config{Routes: map[string]config.Route{}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(handler.Close)

	stub := &usageQueueCoreStub{records: []json.RawMessage{
		json.RawMessage(`{"timestamp":"2026-11-06T09:00:00Z","latency_ms":1200,"ttft_ms":180,"tokens":{"input_tokens":10,"output_tokens":25,"total_tokens":35},"failed":false,"stream":true,"provider":"claude","model":"claude-sonnet","alias":"fast","auth_index":"claude-account-1","request_id":"trace-1","execution_id":"exec-1","endpoint":"/v1/messages","source":"oauth","api_key":"credential-secret","access_token_sha256":"token-hash-secret","client_ip":"192.0.2.10"}`),
		json.RawMessage(`{"timestamp":"2026-11-06T09:01:00Z","latency_ms":400,"tokens":{"input_tokens":4,"output_tokens":0},"failed":true,"fail":{"status_code":429},"provider":"codex","model":"codex-latest","auth_index":"codex-account-2","execution_id":"exec-2"}`),
	}}
	handler.core = stub

	if err := handler.collectCoreUsageBatch(context.Background()); err != nil {
		t.Fatalf("collectCoreUsageBatch() error = %v", err)
	}
	if len(handler.store.snapshot().Requests) != 2 {
		t.Fatalf("persisted request count = %d, want 2", len(handler.store.snapshot().Requests))
	}

	rows := handler.store.snapshot().Requests
	first := rows[0]
	if first.ID != "exec-1" || first.Model != "fast" || first.Provider != "claude" {
		t.Fatalf("unexpected normalized usage identity: %#v", first)
	}
	if first.Engine != "cliproxy" || first.Account != "claude-account-1" || first.Endpoint != "/v1/messages" {
		t.Fatalf("runtime/account metadata not retained: %#v", first)
	}
	if first.Status != http.StatusOK || first.PromptTokens != 10 || first.CompletionTokens != 25 || first.TotalTokens != 35 {
		t.Fatalf("usage accounting fields not retained: %#v", first)
	}
	if first.TTFTMS != 180 || !first.Streaming {
		t.Fatalf("stream timing metadata not retained: %#v", first)
	}
	if rows[1].Status != http.StatusTooManyRequests || rows[1].Engine != "cliproxy" {
		t.Fatalf("failed runtime event was not normalized: %#v", rows[1])
	}

	snapshotJSON, err := json.Marshal(handler.store.snapshot())
	if err != nil {
		t.Fatalf("marshal safe request history: %v", err)
	}
	if strings.Contains(string(snapshotJSON), "credential-secret") || strings.Contains(string(snapshotJSON), "token-hash-secret") || strings.Contains(string(snapshotJSON), "192.0.2.10") {
		t.Fatalf("sensitive SDK usage fields leaked into Fantom history: %s", snapshotJSON)
	}
}

func TestHealthAndAuthenticatedChatProxy(t *testing.T) {
	t.Setenv("FANTOM_DATA_DIR", t.TempDir())
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" { t.Errorf("unexpected upstream path: %s", r.URL.Path) }
		if r.Header.Get("Authorization") != "Bearer upstream-secret" { t.Errorf("upstream authorization header was not set") }
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil { t.Errorf("decode upstream request: %v", err) }
		if payload["model"] != "test-model" { t.Errorf("unexpected upstream model: %#v", payload["model"]) }
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chat-test","choices":[]}`)
	}))
	defer upstream.Close()

	cfg := &config.Config{
		DefaultProvider: "test",
		GatewayAPIKeys: []string{"client-secret"},
		MaxRequestBytes: 1 << 20,
		Providers: []config.Provider{{
			ID: "test", BaseURL: upstream.URL + "/v1", AuthType: "bearer",
			APIKeys: []string{"upstream-secret"}, Models: []string{"test-model"}, TimeoutSeconds: 5,
		}},
	}
	handler := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(handler.Close)

	health := httptest.NewRecorder()
	handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if health.Code != http.StatusOK { t.Fatalf("health status = %d, want 200", health.Code) }

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if unauthorized.Code != http.StatusUnauthorized { t.Fatalf("unauthorized status = %d, want 401", unauthorized.Code) }

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test-model","messages":[]}`))
	request.Header.Set("Authorization", "Bearer client-secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK { t.Fatalf("chat status = %d, body=%s", response.Code, response.Body.String()) }
	if !strings.Contains(response.Body.String(), "chat-test") { t.Fatalf("unexpected response body: %s", response.Body.String()) }
}

func TestOpenCodeConsoleUsesItsOwnChatAndModelsURLs(t *testing.T) {
	t.Setenv("FANTOM_DATA_DIR", t.TempDir())
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer oc_sk_test" {
			t.Errorf("OpenCode Console API key not forwarded")
		}
		if r.Header.Get("x-opencode-session") != "ses_test_conversation" && r.URL.Path == "/inference/openai/v1/chat/completions" {
			t.Errorf("stable OpenCode session not forwarded")
		}
		switch r.URL.Path {
		case "/inference/v1/models":
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"kimi-k3"},{"id":"some-free-model"}]}`)
		case "/inference/openai/v1/chat/completions":
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil { t.Errorf("decode request: %v", err) }
			messages := payload["messages"].([]any)
			if messages[0].(map[string]any)["role"] != "system" {
				t.Errorf("requested code mode was not injected: %#v", messages[0])
			}
			_, _ = io.WriteString(w, `{"id":"opencode-chat","choices":[]}`)
		default:
			t.Errorf("unexpected OpenCode Console path: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	cfg := &config.Config{
		DefaultProvider: "opencode-console",
		GatewayAPIKeys: []string{"gateway-secret"},
		MaxRequestBytes: 1 << 20,
		Providers: []config.Provider{{
			ID: "opencode-console",
			BaseURL: upstream.URL + "/inference",
			ChatPath: "/openai/v1/chat/completions",
			ModelsURL: upstream.URL + "/inference/v1/models",
			AuthType: "bearer",
			APIKeys: []string{"oc_sk_test"},
			Models: []string{"kimi-k3"},
			TimeoutSeconds: 5,
		}},
	}
	handler := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(handler.Close)

	modelRequest := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	modelRequest.Header.Set("Authorization", "Bearer gateway-secret")
	modelResponse := httptest.NewRecorder()
	handler.ServeHTTP(modelResponse, modelRequest)
	if modelResponse.Code != http.StatusOK { t.Fatalf("models status = %d, body=%s", modelResponse.Code, modelResponse.Body.String()) }
	if !strings.Contains(modelResponse.Body.String(), "kimi-k3") || strings.Contains(modelResponse.Body.String(), "some-free-model") {
		t.Fatalf("model allowlist was not enforced: %s", modelResponse.Body.String())
	}

	chatRequest := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"kimi-k3","messages":[{"role":"user","content":"Fix this code"}]}`))
	chatRequest.Header.Set("Authorization", "Bearer gateway-secret")
	chatRequest.Header.Set("X-Fantom-Token-Saver", "off")
	chatRequest.Header.Set("X-Fantom-Code-Mode", "full")
	chatRequest.Header.Set("X-OpenCode-Session", "ses_test_conversation")
	chatResponse := httptest.NewRecorder()
	handler.ServeHTTP(chatResponse, chatRequest)
	if chatResponse.Code != http.StatusOK { t.Fatalf("chat status = %d, body=%s", chatResponse.Code, chatResponse.Body.String()) }
	if !strings.Contains(chatResponse.Body.String(), "opencode-chat") { t.Fatalf("unexpected response: %s", chatResponse.Body.String()) }
}


func TestAdminProviderCRUDRoutesAndGeneratedKeys(t *testing.T) {
	t.Setenv("FANTOM_DATA_DIR", t.TempDir())
	cfg := &config.Config{
		DefaultProvider: "nvidia",
		GatewayAPIKeys: []string{"admin-secret"},
		MaxRequestBytes: 1 << 20,
		DefaultTokenSaver: "safe",
		DefaultCodeMode: "off",
		Providers: []config.Provider{{
			ID: "nvidia", BaseURL: "https://example.invalid/v1", AuthType: "bearer",
			APIKeys: []string{"upstream-secret"}, Models: []string{"*"}, TimeoutSeconds: 100,
		}},
		Routes: map[string]config.Route{},
	}
	handler := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(handler.Close)

	call := func(method, path, body, key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		if body != "" { req.Header.Set("Content-Type", "application/json") }
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		return res
	}

	add := call(http.MethodPost, "/api/admin/providers", `{"id":"custom-openai","base_url":"https://api.example.com/v1","chat_path":"/chat/completions","auth_type":"bearer","api_key":"provider-secret","models":["model-a","model-b"],"timeout_seconds":100}`, "admin-secret")
	if add.Code != http.StatusOK { t.Fatalf("provider create = %d: %s", add.Code, add.Body.String()) }

	routes := call(http.MethodPut, "/api/admin/routes", `{"routes":{"fast":{"provider":"custom-openai","upstream_model":"model-a"}}}`, "admin-secret")
	if routes.Code != http.StatusOK { t.Fatalf("route update = %d: %s", routes.Code, routes.Body.String()) }

	models := call(http.MethodGet, "/v1/models", "", "admin-secret")
	if models.Code != http.StatusOK || !strings.Contains(models.Body.String(), "\"fast\"") {
		t.Fatalf("routed model missing from catalog: %d %s", models.Code, models.Body.String())
	}

	created := call(http.MethodPost, "/api/admin/keys", `{"name":"CI test key"}`, "admin-secret")
	if created.Code != http.StatusCreated { t.Fatalf("key create = %d: %s", created.Code, created.Body.String()) }
	var keyData map[string]any
	if err := json.Unmarshal(created.Body.Bytes(), &keyData); err != nil { t.Fatal(err) }
	clientKey, ok := keyData["key"].(string)
	if !ok || !strings.HasPrefix(clientKey, "ftm_live_") { t.Fatalf("generated key was not returned once: %#v", keyData) }
	clientAuth := call(http.MethodGet, "/v1/models", "", clientKey)
	if clientAuth.Code != http.StatusOK { t.Fatalf("generated client key cannot access model API: %d %s", clientAuth.Code, clientAuth.Body.String()) }
	adminDenied := call(http.MethodGet, "/api/admin/bootstrap", "", clientKey)
	if adminDenied.Code != http.StatusUnauthorized { t.Fatalf("generated client key must not access admin API: %d %s", adminDenied.Code, adminDenied.Body.String()) }

	settings := call(http.MethodPut, "/api/admin/settings", `{"default_provider":"custom-openai","token_saver":"safe","code_mode":"full"}`, "admin-secret")
	if settings.Code != http.StatusOK { t.Fatalf("settings update = %d: %s", settings.Code, settings.Body.String()) }

	removed := call(http.MethodDelete, "/api/admin/providers/custom-openai", "", "admin-secret")
	if removed.Code != http.StatusOK { t.Fatalf("provider delete = %d: %s", removed.Code, removed.Body.String()) }
}


func TestComboStrategyValidationAndLegacyDefault(t *testing.T) {
	t.Setenv("FANTOM_DATA_DIR", t.TempDir())
	cfg := &config.Config{
		DefaultProvider: "primary",
		GatewayAPIKeys: []string{"admin-secret"},
		Providers: []config.Provider{{
			ID: "primary", BaseURL: "https://example.invalid/v1",
			AuthType: "bearer", APIKeys: []string{"provider-secret"},
			Models: []string{"model-a"}, TimeoutSeconds: 5,
		}},
	}
	handler := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(handler.Close)

	if err := handler.store.setRoutes(map[string]config.Route{
		"legacy-combo": {Provider: "primary", UpstreamModel: "model-a"},
		"rr-combo": {Provider: "primary", UpstreamModel: "model-a", Strategy: "ROUND-ROBIN"},
	}); err != nil {
		t.Fatalf("setRoutes() rejected supported strategies: %v", err)
	}
	routes := handler.store.snapshot().Routes
	if routes["legacy-combo"].Strategy != "fallback" {
		t.Fatalf("legacy route strategy = %q, want fallback", routes["legacy-combo"].Strategy)
	}
	if routes["rr-combo"].Strategy != "round-robin" {
		t.Fatalf("round-robin strategy = %q, want normalized round-robin", routes["rr-combo"].Strategy)
	}
	if err := handler.store.setRoutes(map[string]config.Route{
		"invalid": {Provider: "primary", UpstreamModel: "model-a", Strategy: "smartest"},
	}); err == nil {
		t.Fatal("setRoutes() accepted an unsupported combo strategy")
	}
}

func TestRoundRobinCandidateOffsetHonorsStickyLimit(t *testing.T) {
	var cursor atomic.Uint64
	want := []int{0, 0, 1, 1, 2, 2, 0, 0}
	for index, expected := range want {
		if got := roundRobinCandidateOffset(&cursor, 2, 3); got != expected {
			t.Fatalf("request %d candidate offset = %d, want %d", index+1, got, expected)
		}
	}
}

func TestRoundRobinComboRotatesCandidates(t *testing.T) {
	t.Setenv("FANTOM_DATA_DIR", t.TempDir())

	type upstreamCounter struct {
		model string
		count atomic.Int64
	}
	counters := []*upstreamCounter{
		{model: "primary-model"},
		{model: "backup-model-a"},
		{model: "backup-model-b"},
	}
	servers := make([]*httptest.Server, 0, len(counters))
	providers := make([]config.Provider, 0, len(counters))
	for index, counter := range counters {
		counter := counter
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Errorf("decode routed request: %v", err)
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			if got := payload["model"]; got != counter.model {
				t.Errorf("upstream model = %#v, want %q", got, counter.model)
			}
			counter.count.Add(1)
			_, _ = io.WriteString(w, `{"id":"round-robin-success","choices":[]}`)
		}))
		servers = append(servers, server)
		defer server.Close()
		providerID := []string{"primary", "backup-a", "backup-b"}[index]
		providers = append(providers, config.Provider{
			ID: providerID, BaseURL: server.URL + "/v1", AuthType: "none",
			Models: []string{counter.model}, TimeoutSeconds: 5,
		})
	}

	cfg := &config.Config{
		DefaultProvider: "primary",
		GatewayAPIKeys: []string{"gateway-secret"},
		MaxRequestBytes: 1 << 20,
		DefaultTokenSaver: "off",
		DefaultCodeMode: "off",
		Providers: providers,
		Routes: map[string]config.Route{
			"frontdoor": {
				Provider: "primary", UpstreamModel: "primary-model",
				FallbackModels: []string{"backup-a", "backup-b"}, Strategy: "round-robin",
			},
			"backup-a": {Provider: "backup-a", UpstreamModel: "backup-model-a"},
			"backup-b": {Provider: "backup-b", UpstreamModel: "backup-model-b"},
		},
	}
	handler := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(handler.Close)

	for requestIndex := 0; requestIndex < 6; requestIndex++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
			`{"model":"frontdoor","messages":[{"role":"user","content":"hello"}]}`,
		))
		req.Header.Set("Authorization", "Bearer gateway-secret")
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		if res.Code != http.StatusOK {
			t.Fatalf("round-robin request %d = %d: %s", requestIndex+1, res.Code, res.Body.String())
		}
	}
	for _, counter := range counters {
		if got := counter.count.Load(); got != 2 {
			t.Errorf("provider for %s received %d requests, want 2", counter.model, got)
		}
	}
}

func TestRouteFallbackMovesToNextProviderOnRateLimit(t *testing.T) {
	t.Setenv("FANTOM_DATA_DIR", t.TempDir())
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil { t.Errorf("decode primary request: %v", err) }
		if payload["model"] != "primary-model" { t.Errorf("primary model = %#v", payload["model"]) }
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"rate limited"}}`)
	}))
	defer primary.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil { t.Errorf("decode fallback request: %v", err) }
		if payload["model"] != "backup-model" { t.Errorf("fallback model = %#v", payload["model"]) }
		_, _ = io.WriteString(w, `{"id":"fallback-success","choices":[]}`)
	}))
	defer fallback.Close()

	cfg := &config.Config{
		DefaultProvider: "primary",
		GatewayAPIKeys: []string{"gateway-secret"},
		MaxRequestBytes: 1 << 20,
		DefaultTokenSaver: "off",
		DefaultCodeMode: "off",
		Providers: []config.Provider{
			{ID:"primary",BaseURL:primary.URL+"/v1",AuthType:"bearer",APIKeys:[]string{"primary-key"},Models:[]string{"primary-model"},TimeoutSeconds:5},
			{ID:"backup",BaseURL:fallback.URL+"/v1",AuthType:"bearer",APIKeys:[]string{"backup-key"},Models:[]string{"backup-model"},TimeoutSeconds:5},
		},
		Routes: map[string]config.Route{
			"frontdoor": {Provider:"primary",UpstreamModel:"primary-model",FallbackModels:[]string{"backup"}},
			"backup": {Provider:"backup",UpstreamModel:"backup-model"},
		},
	}
	handler:=New(cfg,slog.New(slog.NewTextHandler(io.Discard,nil)))
	t.Cleanup(handler.Close)
	req:=httptest.NewRequest(http.MethodPost,"/v1/chat/completions",strings.NewReader(`{"model":"frontdoor","messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Authorization","Bearer gateway-secret")
	res:=httptest.NewRecorder()
	handler.ServeHTTP(res,req)
	if res.Code!=http.StatusOK { t.Fatalf("fallback response = %d: %s",res.Code,res.Body.String()) }
	if !strings.Contains(res.Body.String(),"fallback-success") { t.Fatalf("fallback response body = %s",res.Body.String()) }
	if res.Header().Get("X-Fantom-Selected-Provider")!="backup" { t.Fatalf("selected provider = %q",res.Header().Get("X-Fantom-Selected-Provider")) }
	if res.Header().Get("X-Fantom-Fallback-Attempts")!="1" { t.Fatalf("fallback attempts = %q",res.Header().Get("X-Fantom-Fallback-Attempts")) }
}


func TestProviderRuntimeRotatesUpstreamKeysRoundRobin(t *testing.T) {
	provider := &providerRuntime{config:config.Provider{APIKeys:[]string{"key-a","key-b"}}}
	got:=[]string{provider.apiKey(),provider.apiKey(),provider.apiKey(),provider.apiKey()}
	want:=[]string{"key-a","key-b","key-a","key-b"}
	for i:=range want { if got[i]!=want[i] { t.Fatalf("key rotation = %#v, want %#v",got,want) } }
}


func TestFallbackEligibleStatuses(t *testing.T) {
	for _,status:=range []int{http.StatusUnauthorized,http.StatusForbidden,http.StatusRequestTimeout,http.StatusConflict,425,http.StatusTooManyRequests,http.StatusInternalServerError,http.StatusBadGateway,http.StatusServiceUnavailable,http.StatusGatewayTimeout} {
		if !shouldFallback(status) { t.Errorf("status %d should allow fallback",status) }
	}
	for _,status:=range []int{http.StatusBadRequest,http.StatusNotFound,http.StatusUnprocessableEntity,http.StatusInternalServerError+1} {
		if shouldFallback(status) { t.Errorf("status %d should not allow fallback",status) }
	}
}


// A rejected credential must not prevent another credential in the same provider
// pool from serving the request.
func TestChatRetriesNextProviderCredentialAfterUnauthorized(t *testing.T) {
	t.Setenv("FANTOM_DATA_DIR",t.TempDir())
	var seen []string
	upstream:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request) {
		auth:=r.Header.Get("Authorization")
		seen=append(seen,auth)
		if auth=="Bearer revoked-key" { w.WriteHeader(http.StatusUnauthorized); _,_=io.WriteString(w,`{"error":{"message":"revoked"}}`); return }
		w.Header().Set("X-Fantom-Upstream-Attempts", "999")
		w.Header().Set("X-Fantom-Selected-Provider", "spoofed-provider")
		w.Header().Set("X-Request-ID", "spoofed-request")
		_,_=io.WriteString(w,`{"id":"key-failover-ok","choices":[]}`)
	}))
	defer upstream.Close()
	cfg:=&config.Config{
		DefaultProvider:"primary",GatewayAPIKeys:[]string{"gateway-secret"},MaxRequestBytes:1<<20,
		Providers:[]config.Provider{{ID:"primary",BaseURL:upstream.URL+"/v1",AuthType:"bearer",APIKeys:[]string{"revoked-key","healthy-key"},Models:[]string{"test-model"},TimeoutSeconds:5}},
	}
	handler:=New(cfg,slog.New(slog.NewTextHandler(io.Discard,nil)))
	t.Cleanup(handler.Close)
	req:=httptest.NewRequest(http.MethodPost,"/v1/chat/completions",strings.NewReader(`{"model":"test-model","messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Authorization","Bearer gateway-secret")
	res:=httptest.NewRecorder()
	handler.ServeHTTP(res,req)
	if res.Code!=http.StatusOK { t.Fatalf("response = %d: %s",res.Code,res.Body.String()) }
	if len(seen)!=2 || seen[0]!="Bearer revoked-key" || seen[1]!="Bearer healthy-key" {
		t.Fatalf("credential retry order = %#v",seen)
	}
	if res.Header().Get("X-Fantom-Upstream-Attempts") != "2" {
		t.Fatalf("upstream attempts header = %q", res.Header().Get("X-Fantom-Upstream-Attempts"))
	}
	if res.Header().Get("X-Fantom-Credential-Retries") != "1" {
		t.Fatalf("credential retry header = %q", res.Header().Get("X-Fantom-Credential-Retries"))
	}
	if got := res.Header().Get("X-Fantom-Fallback-Attempts"); got != "" {
		t.Fatalf("credential retries must not be reported as model fallbacks: %q", got)
	}
	if values := res.Header().Values("X-Fantom-Upstream-Attempts"); len(values) != 1 || values[0] != "2" {
		t.Fatalf("upstream must not spoof gateway attempt header: %#v", values)
	}
	if got := res.Header().Get("X-Fantom-Selected-Provider"); got != "primary" {
		t.Fatalf("upstream must not spoof selected provider: %q", got)
	}
	if got := res.Header().Get("X-Request-ID"); strings.Contains(got, "spoofed") {
		t.Fatalf("upstream must not spoof request ID: %q", got)
	}

	logReq := httptest.NewRequest(http.MethodGet, "/api/admin/logs?limit=1", nil)
	logReq.Header.Set("Authorization", "Bearer gateway-secret")
	logResponse := httptest.NewRecorder()
	handler.ServeHTTP(logResponse, logReq)
	var history struct {
		Requests []requestRecord `json:"requests"`
	}
	if err := json.Unmarshal(logResponse.Body.Bytes(), &history); err != nil {
		t.Fatal(err)
	}
	if len(history.Requests) != 1 {
		t.Fatalf("request history length = %d, want 1", len(history.Requests))
	}
	row := history.Requests[0]
	if row.UpstreamAttempts != 2 || row.CredentialRetries != 1 || row.FallbackAttempts != 0 {
		t.Fatalf("incorrect retry diagnostics persisted: %+v", row)
	}
}

func TestCaptureProviderUsageFromJSONAndSSE(t *testing.T) {
	jsonRecord:=requestRecord{}
	captureProviderUsage([]byte(`{"usage":{"prompt_tokens":12,"completion_tokens":7,"total_tokens":19}}`),"application/json",&jsonRecord)
	if jsonRecord.PromptTokens!=12 || jsonRecord.CompletionTokens!=7 || jsonRecord.TotalTokens!=19 {
		t.Fatalf("JSON usage was not captured: %+v",jsonRecord)
	}
	sseRecord:=requestRecord{}
	sse:=[]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":20}}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":9}}\n\n")
	captureProviderUsage(sse,"text/event-stream",&sseRecord)
	if sseRecord.PromptTokens!=20 || sseRecord.CompletionTokens!=9 || sseRecord.TotalTokens!=29 {
		t.Fatalf("SSE usage was not captured: %+v",sseRecord)
	}
}

func TestProviderTestRequiresChatCompletionNotJustModelsEndpoint(t *testing.T) {
	t.Setenv("FANTOM_DATA_DIR",t.TempDir())
	upstream:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_,_=io.WriteString(w,`{"data":[{"id":"smoke-model"}]}`)
		case "/v1/chat/completions":
			_,_=io.WriteString(w,`{"id":"smoke-ok","choices":[{"message":{"content":"OK"}}]}`)
		default: http.NotFound(w,r)
		}
	}))
	defer upstream.Close()
	cfg:=&config.Config{
		DefaultProvider:"primary",GatewayAPIKeys:[]string{"admin-secret"},MaxRequestBytes:1<<20,
		Providers:[]config.Provider{{ID:"primary",BaseURL:upstream.URL+"/v1",AuthType:"none",Models:[]string{"*"},TimeoutSeconds:5}},
	}
	handler:=New(cfg,slog.New(slog.NewTextHandler(io.Discard,nil)))
	t.Cleanup(handler.Close)
	req:=httptest.NewRequest(http.MethodPost,"/api/admin/providers/primary/test",strings.NewReader("{}"))
	req.Header.Set("Authorization","Bearer admin-secret")
	res:=httptest.NewRecorder()
	handler.ServeHTTP(res,req)
	if res.Code!=http.StatusOK { t.Fatalf("test endpoint = %d: %s",res.Code,res.Body.String()) }
	if !strings.Contains(res.Body.String(),`"chat_ok":true`) || !strings.Contains(res.Body.String(),`"test_model":"smoke-model"`) {
		t.Fatalf("provider smoke test did not verify chat completion: %s",res.Body.String())
	}
}


func TestModelCatalogIsCachedAndCanBeInvalidated(t *testing.T) {
	t.Setenv("FANTOM_DATA_DIR",t.TempDir())
	var catalogCalls atomic.Int32
	upstream:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request) {
		if r.URL.Path!="/v1/models" { http.NotFound(w,r); return }
		catalogCalls.Add(1)
		_,_=io.WriteString(w,`{"data":[{"id":"cached-model"}]}`)
	}))
	defer upstream.Close()
	cfg:=&config.Config{
		DefaultProvider:"primary",GatewayAPIKeys:[]string{"admin-secret"},MaxRequestBytes:1<<20,
		Providers:[]config.Provider{{ID:"primary",BaseURL:upstream.URL+"/v1",AuthType:"none",Models:[]string{"*"},TimeoutSeconds:5}},
	}
	handler:=New(cfg,slog.New(slog.NewTextHandler(io.Discard,nil)))
	t.Cleanup(handler.Close)
	call:=func(method,path string)*httptest.ResponseRecorder{
		req:=httptest.NewRequest(method,path,nil)
		req.Header.Set("Authorization","Bearer admin-secret")
		res:=httptest.NewRecorder(); handler.ServeHTTP(res,req); return res
	}
	for i:=0;i<2;i++ {
		res:=call(http.MethodGet,"/v1/models")
		if res.Code!=http.StatusOK || !strings.Contains(res.Body.String(),"cached-model") { t.Fatalf("model list %d = %d: %s",i,res.Code,res.Body.String()) }
	}
	if got:=catalogCalls.Load();got!=1 { t.Fatalf("catalog endpoint called %d times before expiry, want 1",got) }
	refresh:=call(http.MethodPost,"/api/admin/models/refresh")
	if refresh.Code!=http.StatusOK { t.Fatalf("catalog refresh = %d: %s",refresh.Code,refresh.Body.String()) }
	_ = call(http.MethodGet,"/v1/models")
	if got:=catalogCalls.Load();got!=2 { t.Fatalf("catalog endpoint called %d times after refresh, want 2",got) }
}


func TestProviderCooldownSkipsThrottledCredentials(t *testing.T) {
	provider := &providerRuntime{config: config.Provider{APIKeys: []string{"throttled-key", "healthy-key"}}}
	provider.cooldownCredential("throttled-key", http.StatusTooManyRequests, "120")

	got := provider.apiKeysForRequest()
	if len(got) != 1 || got[0] != "healthy-key" {
		t.Fatalf("available key order = %#v, want only healthy-key", got)
	}

	// If every key is cooling down, do not fan out to every known-throttled key.
	provider.cooldownCredential("healthy-key", http.StatusForbidden, "")
	got = provider.apiKeysForRequest()
	if len(got) != 0 {
		t.Fatalf("all-cooling pool should not probe an active cooldown, got %#v", got)
	}
	if remaining := provider.credentialCooldownRemaining(); remaining <= 0 {
		t.Fatalf("expected a positive retry delay, got %s", remaining)
	}
}

func TestCredentialCooldownHonorsAndBoundsRetryAfter(t *testing.T) {
	now := time.Now()
	if got := credentialCooldown(http.StatusTooManyRequests, "120", now); got != 120*time.Second {
		t.Fatalf("Retry-After seconds = %s, want 2m", got)
	}
	if got := credentialCooldown(http.StatusTooManyRequests, "7200", now); got != time.Hour {
		t.Fatalf("large Retry-After was not capped: %s", got)
	}
	if got := credentialCooldown(http.StatusTooManyRequests, "0", now); got != 0 {
		t.Fatalf("Retry-After: 0 should not add a cooldown, got %s", got)
	}
	if got := credentialCooldown(http.StatusUnauthorized, "", now); got != 2*time.Minute {
		t.Fatalf("unauthorized cooldown = %s, want 2m", got)
	}
	if got := credentialCooldown(http.StatusBadGateway, "", now); got != 0 {
		t.Fatalf("non-credential failure should not cool a key, got %s", got)
	}
}


func TestChatReturnsRetryAfterWhenEveryCredentialIsCoolingDown(t *testing.T) {
	t.Setenv("FANTOM_DATA_DIR", t.TempDir())
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"rate limited"}}`)
	}))
	defer upstream.Close()

	cfg := &config.Config{
		DefaultProvider: "limited",
		GatewayAPIKeys: []string{"gateway-secret"},
		MaxRequestBytes: 1 << 20,
		DefaultTokenSaver: "off",
		DefaultCodeMode: "off",
		Providers: []config.Provider{{
			ID: "limited", BaseURL: upstream.URL + "/v1", AuthType: "bearer",
			APIKeys: []string{"limited-key"}, Models: []string{"test-model"}, TimeoutSeconds: 5,
		}},
		Routes: map[string]config.Route{},
	}
	handler := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(handler.Close)

	call := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test-model","messages":[{"role":"user","content":"hello"}]}`))
		req.Header.Set("Authorization", "Bearer gateway-secret")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}
	first := call()
	if first.Code != http.StatusTooManyRequests {
		t.Fatalf("first response = %d, want 429", first.Code)
	}
	second := call()
	if second.Code != http.StatusServiceUnavailable {
		t.Fatalf("second response = %d, want 503 while key is cooling down: %s", second.Code, second.Body.String())
	}
	if second.Header().Get("Retry-After") == "" {
		t.Fatal("503 response did not include Retry-After")
	}
	if got := upstreamCalls.Load(); got != 1 {
		t.Fatalf("upstream was called %d times, want only once during the cooldown window", got)
	}
}


func TestProviderCooldownMarksDuplicateConfiguredKeys(t *testing.T) {
	provider := &providerRuntime{config: config.Provider{APIKeys: []string{"same-key", "same-key", "healthy-key"}}}
	provider.cooldownCredential("same-key", http.StatusUnauthorized, "")
	got := provider.apiKeysForRequest()
	if len(got) != 1 || got[0] != "healthy-key" {
		t.Fatalf("duplicate credential entries were not cooled together: %#v", got)
	}
}


func TestProviderKeyPoolDoesNotRetryDuplicateSecretTwice(t *testing.T) {
	provider := &providerRuntime{config: config.Provider{APIKeys: []string{"same-key", "same-key", "other-key"}}}
	got := provider.apiKeysForRequest()
	if len(got) != 2 {
		t.Fatalf("unique credential candidates = %#v, want two unique keys", got)
	}
	if got[0] == got[1] {
		t.Fatalf("duplicate secret was scheduled twice: %#v", got)
	}
}
