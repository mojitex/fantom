package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mojitex/fantom/internal/config"
)


func TestUpstreamAdmissionAndWeightedBodyBudget(t *testing.T) {
	server := &Server{
		upstreamSlots: make(chan struct{}, 1),
		maxInflightBodyBytes: 10,
	}
	releaseUpstream, ok := server.tryAcquireUpstream()
	if !ok {
		t.Fatal("first upstream admission unexpectedly failed")
	}
	if _, ok := server.tryAcquireUpstream(); ok {
		t.Fatal("second upstream admission exceeded the global limit")
	}
	releaseUpstream()
	releaseAgain, ok := server.tryAcquireUpstream()
	if !ok {
		t.Fatal("upstream slot was not released")
	}
	releaseAgain()

	releaseBody, ok := server.tryAcquireBodyBytes(7)
	if !ok {
		t.Fatal("first weighted body reservation unexpectedly failed")
	}
	if _, ok := server.tryAcquireBodyBytes(4); ok {
		t.Fatal("body budget allowed reservations above its configured maximum")
	}
	releaseBody()
	releaseBody, ok = server.tryAcquireBodyBytes(10)
	if !ok {
		t.Fatal("body reservation did not recover after release")
	}
	releaseBody()
}


func TestMetricsEndpointRequiresAdministratorKey(t *testing.T) {
	t.Setenv("FANTOM_DATA_DIR", t.TempDir())
	handler := New(&config.Config{
		DefaultProvider: "test",
		GatewayAPIKeys: []string{"admin-secret"},
		MaxRequestBytes: 1 << 20,
		MaxInflightBodyBytes: 2 << 20,
		MaxConcurrentRequests: 4,
		MaxConcurrentUpstreamRequests: 8,
		MaxConcurrentPerProvider: 2,
		Providers: []config.Provider{{ID: "test-provider", BaseURL: "https://provider.example/v1", Models: []string{"test-model"}}},
		Routes: map[string]config.Route{},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(handler.Close)

	unauthorized := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	unauthorizedResponse := httptest.NewRecorder()
	handler.ServeHTTP(unauthorizedResponse, unauthorized)
	if unauthorizedResponse.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized metrics status = %d, want 401", unauthorizedResponse.Code)
	}

	request := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	request.Header.Set("Authorization", "Bearer admin-secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("metrics status = %d: %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, expected := range []string{
		"fantom_inference_limit 4",
		"fantom_upstream_limit 8",
		"fantom_request_body_bytes_limit 2097152",
		"fantom_provider_limit{provider=\"test-provider\"} 2",
	} {
		if !strings.Contains(body, expected) {
			t.Errorf("metrics output missing %q", expected)
		}
	}
}

func TestProviderRuntimeTryAcquireIsBounded(t *testing.T) {
	provider := newProviderRuntime(config.Provider{ID: "bounded"}, 1)
	release, ok := provider.tryAcquire()
	if !ok {
		t.Fatal("first acquire unexpectedly failed")
	}
	if _, ok := provider.tryAcquire(); ok {
		t.Fatal("second acquire exceeded the configured provider limit")
	}
	release()
	releaseAgain, ok := provider.tryAcquire()
	if !ok {
		t.Fatal("acquire after release unexpectedly failed")
	}
	releaseAgain()
}


func TestInferenceAdmissionRejectsWhenBodyBudgetIsExhausted(t *testing.T) {
	t.Setenv("FANTOM_DATA_DIR", t.TempDir())
	upstreamStarted := make(chan struct{}, 1)
	releaseUpstream := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamStarted <- struct{}{}
		<-releaseUpstream
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"body-budget-ok","choices":[]}`)
	}))
	defer upstream.Close()
	handler := New(&config.Config{
		DefaultProvider: "test",
		GatewayAPIKeys: []string{"client-secret"},
		MaxRequestBytes: 1 << 20,
		MaxConcurrentRequests: 4,
		MaxConcurrentUpstreamRequests: 4,
		MaxConcurrentPerProvider: 4,
		MaxInflightBodyBytes: 1 << 20,
		Providers: []config.Provider{{
			ID: "test", BaseURL: upstream.URL, ChatPath: "/v1/chat/completions",
			AuthType: "none", Models: []string{"test-model"}, TimeoutSeconds: 5,
		}},
		Routes: map[string]config.Route{},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(handler.Close)

	makeRequest := func(content string) *httptest.ResponseRecorder {
		body := `{"model":"test-model","messages":[{"role":"user","content":"` + content + `"}]}`
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer client-secret")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { firstDone <- makeRequest(strings.Repeat("a", 700<<10)) }()
	select {
	case <-upstreamStarted:
	case <-time.After(2 * time.Second):
		close(releaseUpstream)
		t.Fatal("first large request did not reach upstream")
	}

	secondDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { secondDone <- makeRequest(strings.Repeat("b", 400<<10)) }()
	var second *httptest.ResponseRecorder
	select {
	case second = <-secondDone:
	case <-time.After(2 * time.Second):
		close(releaseUpstream)
		<-firstDone
		<-secondDone
		t.Fatal("body-budget saturated request did not fail fast")
	}
	if second.Code != http.StatusServiceUnavailable {
		close(releaseUpstream)
		<-firstDone
		t.Fatalf("body budget status = %d, want 503: %s", second.Code, second.Body.String())
	}
	if second.Header().Get("X-Fantom-Admission") != "body-budget" {
		close(releaseUpstream)
		<-firstDone
		t.Fatalf("body budget admission header = %q", second.Header().Get("X-Fantom-Admission"))
	}
	if second.Header().Get("Retry-After") != "1" {
		close(releaseUpstream)
		<-firstDone
		t.Fatalf("body budget Retry-After = %q, want 1", second.Header().Get("Retry-After"))
	}
	close(releaseUpstream)
	select {
	case first := <-firstDone:
		if first.Code != http.StatusOK {
			t.Fatalf("first large request status = %d: %s", first.Code, first.Body.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first large request did not complete after release")
	}
}

func TestInferenceAdmissionRejectsAtGlobalLimit(t *testing.T) {
	assertConcurrentAdmission(t, 1, 4, "global")
}

func TestInferenceAdmissionRejectsAtProviderLimit(t *testing.T) {
	assertConcurrentAdmission(t, 2, 1, "provider")
}

func assertConcurrentAdmission(t *testing.T, maxRequests, maxPerProvider int, limitKind string) {
	t.Helper()
	t.Setenv("FANTOM_DATA_DIR", t.TempDir())

	upstreamStarted := make(chan struct{}, 1)
	releaseUpstream := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamStarted <- struct{}{}
		<-releaseUpstream
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"capacity-ok","choices":[]}`)
	}))
	defer upstream.Close()

	cfg := &config.Config{
		DefaultProvider: "test",
		GatewayAPIKeys: []string{"client-secret"},
		MaxRequestBytes: 1 << 20,
		MaxConcurrentRequests: maxRequests,
		MaxConcurrentUpstreamRequests: 4,
		MaxConcurrentPerProvider: maxPerProvider,
		Providers: []config.Provider{{
			ID: "test", BaseURL: upstream.URL, ChatPath: "/v1/chat/completions",
			AuthType: "none", Models: []string{"test-model"}, TimeoutSeconds: 5,
		}},
		Routes: map[string]config.Route{},
	}
	handler := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(handler.Close)

	makeRequest := func() *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"test-model","messages":[{"role":"user","content":"hello"}]}`))
		request.Header.Set("Authorization", "Bearer client-secret")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { firstDone <- makeRequest() }()
	select {
	case <-upstreamStarted:
	case <-time.After(2 * time.Second):
		close(releaseUpstream)
		t.Fatal("first request never reached the upstream")
	}

	secondDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { secondDone <- makeRequest() }()
	var second *httptest.ResponseRecorder
	select {
	case second = <-secondDone:
	case <-time.After(2 * time.Second):
		close(releaseUpstream)
		<-firstDone
		<-secondDone
		t.Fatalf("%s saturated request did not fail fast", limitKind)
	}
	if second.Code != http.StatusServiceUnavailable {
		close(releaseUpstream)
		<-firstDone
		t.Fatalf("%s saturation status = %d, want 503: %s", limitKind, second.Code, second.Body.String())
	}
	if got := second.Header().Get("Retry-After"); got != "1" {
		close(releaseUpstream)
		<-firstDone
		t.Fatalf("%s saturation Retry-After = %q, want 1", limitKind, got)
	}
	if limitKind == "global" && second.Header().Get("X-Fantom-Admission") != "rejected" {
		close(releaseUpstream)
		<-firstDone
		t.Fatal("global admission rejection header is missing")
	}

	close(releaseUpstream)
	select {
	case first := <-firstDone:
		if first.Code != http.StatusOK {
			t.Fatalf("first request status = %d, want 200: %s", first.Code, first.Body.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first request did not complete after upstream release")
	}
}

func TestStreamBodyWithUsageParsesSSEIncrementally(t *testing.T) {
	server := &Server{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	record := &requestRecord{}
	largeEvent := "data:" + strings.Repeat("x", maxSSEUsageLine) + "\n"
	usageEvent := "data: {\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":11,\"total_tokens\":18}}\n\n"
	input := largeEvent + usageEvent
	response := httptest.NewRecorder()

	server.streamBodyWithUsage(response, strings.NewReader(input), record, "text/event-stream")

	if response.Body.String() != input {
		t.Fatal("SSE body was changed while extracting usage")
	}
	if record.PromptTokens != 7 || record.CompletionTokens != 11 || record.TotalTokens != 18 {
		t.Fatalf("incremental usage was not extracted: %+v", record)
	}
}

func TestAdminStoreRingBufferRetainsNewestHistoryInOrder(t *testing.T) {
	store := &adminStore{data: adminState{
		Routes: map[string]config.Route{"fast": {Provider: "test", UpstreamModel: "model"}},
		Requests: make([]requestRecord, 0, 5000),
		ClientKeys: []storedClientKey{{ID: "key-1"}},
	}}
	rows := make([]requestRecord, 5003)
	for i := range rows {
		rows[i] = requestRecord{ID: fmt.Sprint(i)}
	}
	store.appendRequests(rows)

	snapshot := store.snapshot()
	if len(snapshot.Requests) != 5000 {
		t.Fatalf("retained requests = %d, want 5000", len(snapshot.Requests))
	}
	if snapshot.Requests[0].ID != "3" || snapshot.Requests[len(snapshot.Requests)-1].ID != "5002" {
		t.Fatalf("request history is not ordered oldest-to-newest: first=%q last=%q",
			snapshot.Requests[0].ID, snapshot.Requests[len(snapshot.Requests)-1].ID)
	}
	routing := store.routingSnapshot()
	if routing.Requests != nil || routing.ClientKeys != nil {
		t.Fatal("routing snapshot unexpectedly copied request history or client keys")
	}
	if len(routing.Routes) != 1 {
		t.Fatalf("routing snapshot lost routes: %#v", routing.Routes)
	}

	store.path = filepath.Join(t.TempDir(), "fantom-state.json")
	store.mu.Lock()
	err := store.saveLocked()
	store.mu.Unlock()
	if err != nil {
		t.Fatalf("persist ring-buffer snapshot: %v", err)
	}
	raw, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatal(err)
	}
	var persisted adminState
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatal(err)
	}
	if len(persisted.Requests) != 5000 || persisted.Requests[0].ID != "3" || persisted.Requests[4999].ID != "5002" {
		t.Fatalf("persisted history is not ordered and bounded: count=%d first=%q last=%q",
			len(persisted.Requests), persisted.Requests[0].ID, persisted.Requests[len(persisted.Requests)-1].ID)
	}
}
