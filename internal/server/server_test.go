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
