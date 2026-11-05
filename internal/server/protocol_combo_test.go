package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	cliproxyengine "github.com/mojitex/fantom/internal/cliproxy"
	"github.com/mojitex/fantom/internal/config"
)

type protocolTestCore struct {
	handler http.Handler
}

func (c *protocolTestCore) APIProxy() http.Handler { return c.handler }
func (c *protocolTestCore) ManagementProxy() http.Handler { return http.NotFoundHandler() }
func (c *protocolTestCore) Models(context.Context) ([]cliproxyengine.Model, error) { return nil, nil }
func (c *protocolTestCore) HasModel(context.Context, string) bool { return false }
func (c *protocolTestCore) ModelForProvider(provider, model string) string {
	if strings.TrimSpace(provider) == "" || strings.TrimSpace(model) == "" {
		return ""
	}
	return "fantom-" + provider + "/" + model
}
func (c *protocolTestCore) SyncProviders([]config.Provider) error { return nil }
func (c *protocolTestCore) Close() {}

func TestAnthropicMessagesComboFallsBackAcrossProviders(t *testing.T) {
	t.Setenv("FANTOM_DATA_DIR", t.TempDir())
	var calls []string
	var mu sync.Mutex
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode request: %v", err)
		}
		model, _ := payload["model"].(string)
		mu.Lock()
		calls = append(calls, model)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch model {
		case "fantom-primary/primary-model":
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, "{\"error\":{\"message\":\"rate limited primary\"}}")
		case "fantom-backup/backup-model":
			_, _ = io.WriteString(w, "{\"id\":\"msg_fallback_ok\",\"type\":\"message\",\"model\":\"backup-model\",\"content\":[]}")
		default:
			t.Errorf("unexpected internal model %q", model)
			http.Error(w, "unexpected model", http.StatusBadRequest)
		}
	})

	cfg := &config.Config{
		DefaultProvider: "primary",
		GatewayAPIKeys: []string{"gateway-secret"},
		MaxRequestBytes: 1 << 20,
		Providers: []config.Provider{
			{ID: "primary", BaseURL: "https://primary.example/v1", AuthType: "none", Models: []string{"primary-model"}},
			{ID: "backup", BaseURL: "https://backup.example/v1", AuthType: "none", Models: []string{"backup-model"}},
		},
		Routes: map[string]config.Route{
			"coding-fast": {Provider: "primary", UpstreamModel: "primary-model", FallbackModels: []string{"coding-backup"}},
			"coding-backup": {Provider: "backup", UpstreamModel: "backup-model"},
		},
	}
	handler := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(handler.Close)
	handler.AttachCore(&protocolTestCore{handler: upstream})

	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{\"model\":\"coding-fast\",\"max_tokens\":64,\"messages\":[{\"role\":\"user\",\"content\":\"hello\"}]}"))
	request.Header.Set("Authorization", "Bearer gateway-secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("combo response = %d: %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "msg_fallback_ok") || strings.Contains(response.Body.String(), "rate limited primary") {
		t.Fatalf("unexpected final response body: %s", response.Body.String())
	}
	if got := response.Header().Get("X-Fantom-Upstream-Attempts"); got != "2" {
		t.Fatalf("upstream attempts = %q, want 2", got)
	}
	if got := response.Header().Get("X-Fantom-Fallback-Attempts"); got != "1" {
		t.Fatalf("fallback attempts = %q, want 1", got)
	}
	if got := response.Header().Get("X-Fantom-Selected-Provider"); got != "backup" {
		t.Fatalf("selected provider = %q, want backup", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 || calls[0] != "fantom-primary/primary-model" || calls[1] != "fantom-backup/backup-model" {
		t.Fatalf("internal model sequence = %#v", calls)
	}
}

func TestGeminiComboRewritesModelPath(t *testing.T) {
	t.Setenv("FANTOM_DATA_DIR", t.TempDir())
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1beta/models/fantom-primary/gemini-model:generateContent" {
			t.Errorf("Gemini internal route = %q", r.URL.Path)
			http.Error(w, "incorrect model route", http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, "{\"candidates\":[]}")
	})
	cfg := &config.Config{
		DefaultProvider: "primary",
		GatewayAPIKeys: []string{"gateway-secret"},
		MaxRequestBytes: 1 << 20,
		Providers: []config.Provider{
			{ID: "primary", BaseURL: "https://primary.example/v1", AuthType: "none", Models: []string{"gemini-model"}},
		},
		Routes: map[string]config.Route{
			"gemini-fast": {Provider: "primary", UpstreamModel: "gemini-model"},
		},
	}
	handler := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(handler.Close)
	handler.AttachCore(&protocolTestCore{handler: upstream})

	request := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-fast:generateContent", strings.NewReader("{\"contents\":[]}"))
	request.Header.Set("Authorization", "Bearer gateway-secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("Gemini combo response = %d: %s", response.Code, response.Body.String())
	}
}

func TestProtocolComboDoesNotFallbackOnNonRetryableValidationError(t *testing.T) {
	t.Setenv("FANTOM_DATA_DIR", t.TempDir())
	var calls int
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "{\"error\":{\"message\":\"invalid request\"}}")
	})
	cfg := &config.Config{
		DefaultProvider: "primary",
		GatewayAPIKeys: []string{"gateway-secret"},
		MaxRequestBytes: 1 << 20,
		Providers: []config.Provider{
			{ID: "primary", BaseURL: "https://primary.example/v1", AuthType: "none", Models: []string{"primary-model"}},
			{ID: "backup", BaseURL: "https://backup.example/v1", AuthType: "none", Models: []string{"backup-model"}},
		},
		Routes: map[string]config.Route{
			"fast": {Provider: "primary", UpstreamModel: "primary-model", FallbackModels: []string{"backup-route"}},
			"backup-route": {Provider: "backup", UpstreamModel: "backup-model"},
		},
	}
	handler := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(handler.Close)
	handler.AttachCore(&protocolTestCore{handler: upstream})

	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader("{\"model\":\"fast\",\"input\":\"hello\"}"))
	request.Header.Set("Authorization", "Bearer gateway-secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("validation error status = %d, want 400", response.Code)
	}
	if calls != 1 {
		t.Fatalf("upstream attempts = %d, want 1", calls)
	}
}
