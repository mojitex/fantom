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
	keyAuth := call(http.MethodGet, "/api/admin/bootstrap", "", clientKey)
	if keyAuth.Code != http.StatusOK { t.Fatalf("generated key cannot authenticate: %d %s", keyAuth.Code, keyAuth.Body.String()) }

	settings := call(http.MethodPut, "/api/admin/settings", `{"default_provider":"custom-openai","token_saver":"safe","code_mode":"full"}`, "admin-secret")
	if settings.Code != http.StatusOK { t.Fatalf("settings update = %d: %s", settings.Code, settings.Body.String()) }

	removed := call(http.MethodDelete, "/api/admin/providers/custom-openai", "", "admin-secret")
	if removed.Code != http.StatusOK { t.Fatalf("provider delete = %d: %s", removed.Code, removed.Body.String()) }
}
