package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mojitex/fantom/internal/config"
)

// BenchmarkGatewayChatCompletionsParallel exercises authentication, admission,
// model resolution, provider connection reuse, response accounting and request
// history under parallel load with a deterministic local upstream.
func BenchmarkGatewayChatCompletionsParallel(b *testing.B) {
	b.Setenv("FANTOM_DATA_DIR", b.TempDir())
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"bench","choices":[{"message":{"role":"assistant","content":"OK"}}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`)
	}))
	defer upstream.Close()

	handler := New(&config.Config{
		DefaultProvider: "bench",
		GatewayAPIKeys: []string{"benchmark-client-key"},
		MaxRequestBytes: 1 << 20,
		MaxInflightBodyBytes: 64 << 20,
		MaxConcurrentRequests: 512,
		MaxConcurrentUpstreamRequests: 512,
		MaxConcurrentPerProvider: 512,
		Providers: []config.Provider{{
			ID: "bench", BaseURL: upstream.URL, ChatPath: "/v1/chat/completions",
			AuthType: "none", Models: []string{"bench-model"}, TimeoutSeconds: 10,
		}},
		Routes: map[string]config.Route{},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	b.Cleanup(handler.Close)

	const body = `{"model":"bench-model","messages":[{"role":"user","content":"benchmark request"}],"stream":false}`
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()

	var rejected atomic.Uint64
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
			request.Header.Set("Authorization", "Bearer benchmark-client-key")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				rejected.Add(1)
			}
		}
	})
	b.StopTimer()
	b.ReportMetric(float64(rejected.Load()), "rejected")
}
