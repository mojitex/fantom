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
	"sync/atomic"
	"testing"

	"github.com/mojitex/fantom/internal/config"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestProviderKeyPoolRetriesAfterRateLimitAndRecordsUsage(t *testing.T) {
	t.Setenv("FANTOM_DATA_DIR", t.TempDir())

	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer key-one":
			calls.Add(1)
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"message":"rate limited"}}`)
		case "Bearer key-two":
			calls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"ok","choices":[],"usage":{"prompt_tokens":12,"completion_tokens":8,"total_tokens":20}}`)
		default:
			t.Errorf("unexpected upstream authorization: %q", r.Header.Get("Authorization"))
			http.Error(w, "unexpected key", http.StatusUnauthorized)
		}
	}))
	defer upstream.Close()

	cfg := &config.Config{
		DefaultProvider: "pool",
		GatewayAPIKeys: []string{"gateway-admin"},
		MaxRequestBytes: 1 << 20,
		DefaultTokenSaver: "off",
		DefaultCodeMode: "off",
		Providers: []config.Provider{{
			ID: "pool", BaseURL: upstream.URL + "/v1", AuthType: "bearer",
			APIKeys: []string{"key-one", "key-two"}, Models: []string{"test-model"}, TimeoutSeconds: 5,
		}},
		Routes: map[string]config.Route{},
	}
	handler := New(cfg, testLogger())
	defer handler.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test-model","messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Authorization", "Bearer gateway-admin")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)

	if response.Code != http.StatusOK {
		t.Fatalf("chat status = %d, body=%s", response.Code, response.Body.String())
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream calls = %d, want 2", calls.Load())
	}
	if got := response.Header().Get("X-Fantom-Upstream-Attempts"); got != "2" {
		t.Fatalf("upstream attempts header = %q, want 2", got)
	}
	if got := response.Header().Get("X-Fantom-Fallback-Attempts"); got != "" {
		t.Fatalf("key retry must not be reported as a model fallback, got %q", got)
	}

	usageReq := httptest.NewRequest(http.MethodGet, "/api/admin/usage", nil)
	usageReq.Header.Set("Authorization", "Bearer gateway-admin")
	usageResponse := httptest.NewRecorder()
	handler.ServeHTTP(usageResponse, usageReq)
	if usageResponse.Code != http.StatusOK {
		t.Fatalf("usage status = %d, body=%s", usageResponse.Code, usageResponse.Body.String())
	}
	var usage map[string]int64
	if err := json.Unmarshal(usageResponse.Body.Bytes(), &usage); err != nil {
		t.Fatal(err)
	}
	if usage["prompt_tokens"] != 12 || usage["completion_tokens"] != 8 || usage["total_tokens"] != 20 {
		t.Fatalf("reported usage was not recorded: %#v", usage)
	}
}

func TestStreamingUsageIsRecordedFromSSE(t *testing.T) {
	t.Setenv("FANTOM_DATA_DIR", t.TempDir())
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"stream-1\",\"choices\":[],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":4,\"total_tokens\":13}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	cfg := &config.Config{
		DefaultProvider: "stream",
		GatewayAPIKeys: []string{"gateway-admin"},
		MaxRequestBytes: 1 << 20,
		DefaultTokenSaver: "off",
		DefaultCodeMode: "off",
		Providers: []config.Provider{{
			ID: "stream", BaseURL: upstream.URL + "/v1", AuthType: "none",
			Models: []string{"test-model"}, TimeoutSeconds: 5,
		}},
		Routes: map[string]config.Route{},
	}
	handler := New(cfg, testLogger())
	defer handler.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test-model","stream":true,"messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Authorization", "Bearer gateway-admin")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("chat status = %d, body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "[DONE]") {
		t.Fatalf("SSE body was not transparently forwarded: %s", response.Body.String())
	}

	logReq := httptest.NewRequest(http.MethodGet, "/api/admin/logs?limit=1", nil)
	logReq.Header.Set("Authorization", "Bearer gateway-admin")
	logResponse := httptest.NewRecorder()
	handler.ServeHTTP(logResponse, logReq)
	var data struct {
		Requests []requestRecord `json:"requests"`
	}
	if err := json.Unmarshal(logResponse.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Requests) != 1 || data.Requests[0].TotalTokens != 13 {
		t.Fatalf("streamed usage not present in request log: %#v", data.Requests)
	}
}

func TestRequestHistoryFlushesOnGracefulClose(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FANTOM_DATA_DIR", dir)
	cfg := &config.Config{
		DefaultProvider: "test",
		DefaultTokenSaver: "off",
		DefaultCodeMode: "off",
		Providers: []config.Provider{{
			ID: "test", BaseURL: "https://example.invalid/v1", AuthType: "none",
			Models: []string{"*"}, TimeoutSeconds: 5,
		}},
	}
	store, err := newAdminStore(cfg, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	store.appendRequest(requestRecord{ID: "req-flush", Model: "test-model", Status: http.StatusOK, TotalTokens: 7})
	store.Close()

	raw, err := os.ReadFile(filepath.Join(dir, "fantom-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var state adminState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Requests) != 1 || state.Requests[0].ID != "req-flush" || state.Requests[0].TotalTokens != 7 {
		t.Fatalf("request history was not flushed on close: %#v", state.Requests)
	}
}
