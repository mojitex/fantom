package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mojitex/fantom/internal/config"
)

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
		_,_=io.WriteString(w,`{"id":"key-failover-ok","choices":[]}`)
	}))
	defer upstream.Close()
	cfg:=&config.Config{
		DefaultProvider:"primary",GatewayAPIKeys:[]string{"gateway-secret"},MaxRequestBytes:1<<20,
		Providers:[]config.Provider{{ID:"primary",BaseURL:upstream.URL+"/v1",AuthType:"bearer",APIKeys:[]string{"revoked-key","healthy-key"},Models:[]string{"test-model"},TimeoutSeconds:5}},
	}
	handler:=New(cfg,slog.New(slog.NewTextHandler(io.Discard,nil)))
	req:=httptest.NewRequest(http.MethodPost,"/v1/chat/completions",strings.NewReader(`{"model":"test-model","messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Authorization","Bearer gateway-secret")
	res:=httptest.NewRecorder()
	handler.ServeHTTP(res,req)
	if res.Code!=http.StatusOK { t.Fatalf("response = %d: %s",res.Code,res.Body.String()) }
	if len(seen)!=2 || seen[0]!="Bearer revoked-key" || seen[1]!="Bearer healthy-key" {
		t.Fatalf("credential retry order = %#v",seen)
	}
	if res.Header().Get("X-Fantom-Fallback-Attempts")!="1" {
		t.Fatalf("credential retry attempts header = %q",res.Header().Get("X-Fantom-Fallback-Attempts"))
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
