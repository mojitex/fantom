package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mojitex/fantom/internal/config"
)

func TestServerCloseFlushesRequestHistory(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("FANTOM_DATA_DIR", dataDir)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"id":"lifecycle-ok","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)
	}))
	defer upstream.Close()

	cfg := &config.Config{
		DefaultProvider: "test",
		GatewayAPIKeys: []string{"gateway-secret"},
		MaxRequestBytes: 1 << 20,
		DefaultTokenSaver: "off",
		DefaultCodeMode: "off",
		Providers: []config.Provider{{
			ID: "test", BaseURL: upstream.URL + "/v1", AuthType: "none",
			Models: []string{"test-model"}, TimeoutSeconds: 5,
		}},
		Routes: map[string]config.Route{},
	}
	handler := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test-model","messages":[{"role":"user","content":"hello"}]}`))
	request.Header.Set("Authorization", "Bearer gateway-secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("chat response = %d: %s", response.Code, response.Body.String())
	}

	// The request is queued for batched persistence. Close must synchronously
	// drain the worker before the process exits.
	handler.Close()

	raw, err := os.ReadFile(filepath.Join(dataDir, "fantom-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var state adminState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Requests) != 1 {
		t.Fatalf("persisted requests = %d, want 1", len(state.Requests))
	}
	got := state.Requests[0]
	if got.Model != "test-model" || got.TotalTokens != 5 || got.PromptTokens != 3 || got.CompletionTokens != 2 {
		t.Fatalf("unexpected persisted request: %+v", got)
	}
}
