package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mojitex/fantom/internal/config"
)

func TestRequiredCapabilitiesUsesLatestUserTurn(t *testing.T) {
	payload := map[string]json.RawMessage{
		"messages": json.RawMessage([]byte("[{\"role\":\"user\",\"content\":[{\"type\":\"image_url\",\"image_url\":{\"url\":\"https://example.invalid/old.png\"}}]},{\"role\":\"assistant\",\"content\":\"I can inspect that image.\"},{\"role\":\"user\",\"content\":[{\"type\":\"input_audio\",\"input_audio\":{\"data\":\"AAAA\",\"format\":\"wav\"}}]}]")),
		"tools": json.RawMessage([]byte("[{\"type\":\"function\",\"function\":{\"name\":\"browser_search\"}}]")),
		"reasoning_effort": json.RawMessage([]byte("\"high\"")),
	}
	required := requiredCapabilities(payload)
	for _, key := range []string{"audio_input", "tools", "search", "reasoning"} {
		if !required[key] { t.Errorf("missing capability %q: %#v", key, required) }
	}
	if required["vision"] { t.Fatalf("an earlier user turn affected capability detection: %#v", required) }
}

func TestModelCapabilitiesAndCapacityAdapterResolution(t *testing.T) {
	provider := config.Provider{
		ID: "main",
		Models: []string{"text-model", "vision-model", "adapter-model"},
		ModelCapabilities: map[string]config.ModelCapabilities{
			"vision-model": {Vision: false, Tools: false, ContextWindow: 8192, MaxOutput: 2048},
			"adapter-model": {Vision: true, Tools: true},
			"text-model": {Tools: true},
		},
	}
	caps := modelCapabilities(provider, "public-vision-model", "vision-model")
	if caps.Vision || caps.Tools || caps.ContextWindow != 8192 { t.Fatalf("explicit model metadata not authoritative: %#v", caps) }
	providers := map[string]config.Provider{"main": provider}
	state := adminState{
		DefaultProvider: "main",
		Routes: map[string]config.Route{"text": {Provider: "main", UpstreamModel: "text-model"}},
		CapacityAdapters: map[string]capacityAdapterPool{"vision": {Enabled: true, Models: []string{"main/adapter-model"}}},
	}
	s := &Server{}
	injected := s.applyCapacityAdapters([]string{"text"}, requiredCapabilitySet{"vision": true}, state, providers)
	if len(injected) != 2 || injected[0] != "main/adapter-model" || injected[1] != "text" {
		t.Fatalf("capacity adapter ordering = %#v", injected)
	}
	target, err := resolveCandidate(injected[0], state, providers)
	if err != nil || target.ProviderID != "main" || target.UpstreamModel != "adapter-model" {
		t.Fatalf("provider/model adapter resolution = %#v, %v", target, err)
	}
	eligible, deferred := s.splitHardCompatibleCandidates([]string{"text", "main/adapter-model"}, requiredCapabilitySet{"vision": true}, state, providers)
	if len(eligible) != 1 || eligible[0] != "main/adapter-model" || len(deferred) != 1 || deferred[0] != "text" {
		t.Fatalf("hard-compatible candidates were not separated: eligible=%#v deferred=%#v", eligible, deferred)
	}
	rotated := rotateCandidatesWithinPrefix([]string{"vision-a", "vision-b", "text"}, 1, 2)
	if strings.Join(rotated, ",") != "vision-b,vision-a,text" {
		t.Fatalf("round robin rotated an incompatible fallback: %#v", rotated)
	}
}

func TestCapacityAdapterHistoryTrimmingPreservesInstructionsAndCurrentMedia(t *testing.T) {
	messages:=make([]map[string]any,0,16)
	messages=append(messages,map[string]any{"role":"system","content":strings.Repeat("S",100)})
	for i:=0;i<7;i++ {
		messages=append(messages,map[string]any{"role":"user","content":strings.Repeat("U",500)})
		messages=append(messages,map[string]any{"role":"assistant","content":strings.Repeat("A",500)})
	}
	messages=append(messages,map[string]any{"role":"user","content":[]any{map[string]any{"type":"image_url","image_url":map[string]any{"url":"data:image/png;base64,AAAA"}}}})
	raw,_:=json.Marshal(messages)
	payload:=map[string]json.RawMessage{"messages":raw}
	trimmed,dropped:=stripHistoryForContext(payload,700)
	if dropped==0 {t.Fatal("expected oversized middle history to be trimmed")}
	var result []map[string]any
	if err:=json.Unmarshal(trimmed["messages"],&result);err!=nil {t.Fatalf("decode trimmed messages: %v",err)}
	if len(result)<2 || result[0]["role"]!="system" || result[len(result)-1]["role"]!="user" {
		t.Fatalf("system/current user context was not preserved: %#v",result)
	}
	last:=result[len(result)-1]
	content,_:=last["content"].([]any)
	if len(content)!=1 || content[0].(map[string]any)["type"]!="image_url" {
		t.Fatalf("current-turn media block was lost: %#v",last)
	}
	if again,removed:=stripHistoryForContext(trimmed,700);removed!=0 || len(again["messages"])==0 {
		t.Fatalf("second history trim should be idempotent: removed=%d",removed)
	}
}

func TestCapacityAdapterLegacyArrayMigrationAndRoundRobinPool(t *testing.T) {
	t.Setenv("FANTOM_DATA_DIR", t.TempDir())
	var state adminState
	legacy := []byte(`{"version":1,"capacity_adapters":{"vision":["main/vision-a","main/vision-b"]}}`)
	if err := json.Unmarshal(legacy, &state); err != nil {
		t.Fatalf("legacy capacity adapter array did not decode: %v", err)
	}
	pool := state.CapacityAdapters["vision"]
	if !pool.Enabled || pool.RoundRobin || len(pool.Models) != 2 {
		t.Fatalf("legacy capacity pool migration = %#v", pool)
	}

	provider := config.Provider{
		ID:"main", Models:[]string{"vision-a","vision-b"},
		ModelCapabilities:map[string]config.ModelCapabilities{
			"vision-a":{Vision:true},"vision-b":{Vision:true},
		},
	}
	s := &Server{providers:map[string]*providerRuntime{}, capacityCursors:sync.Map{}}
	s.store = &adminStore{data:adminState{Providers:[]persistedProvider{persistProvider(provider)}}}
	state = adminState{
		DefaultProvider:"main",
		Routes:map[string]config.Route{},
		CapacityAdapters:map[string]capacityAdapterPool{"vision":{Enabled:true,RoundRobin:true,Models:[]string{"main/vision-a","main/vision-b"}}},
	}
	providers:=map[string]config.Provider{"main":provider}
	first:=s.applyCapacityAdapters([]string{"text"},requiredCapabilitySet{"vision":true},state,providers)
	second:=s.applyCapacityAdapters([]string{"text"},requiredCapabilitySet{"vision":true},state,providers)
	if first[0] == second[0] {
		t.Fatalf("round-robin capacity pool did not rotate: first=%#v second=%#v",first,second)
	}
}

func TestCapacityPoolPolicyIsPersistedAndNormalized(t *testing.T) {
	t.Setenv("FANTOM_DATA_DIR", t.TempDir())
	store, err := newAdminStore(&config.Config{
		DefaultProvider:"main",
		Providers:[]config.Provider{{ID:"main",BaseURL:"https://example.invalid/v1",AuthType:"none",Models:[]string{"*"}}},
	}, slog.New(slog.NewTextHandler(io.Discard,nil)))
	if err!=nil { t.Fatalf("newAdminStore() error = %v",err) }
	defer store.Close()
	if err:=store.setCapacityAdapters(map[string]capacityAdapterPool{
		"vision":{Enabled:true,RoundRobin:true,Models:[]string{"main/vision-a"," main/vision-a ","main/vision-b"}},
		"pdf":{Enabled:true,RoundRobin:true,Models:nil},
	});err!=nil { t.Fatalf("setCapacityAdapters() error = %v",err) }
	pools:=store.snapshot().CapacityAdapters
	if !pools["vision"].Enabled || !pools["vision"].RoundRobin || len(pools["vision"].Models)!=2 {
		t.Fatalf("vision pool settings were not normalized: %#v",pools["vision"])
	}
	if pools["pdf"].Enabled || pools["pdf"].RoundRobin || len(pools["pdf"].Models)!=0 {
		t.Fatalf("empty pool should be disabled: %#v",pools["pdf"])
	}
	raw,err:=json.Marshal(store.snapshot())
	if err!=nil { t.Fatalf("marshal capacity settings: %v",err) }
	var reloaded adminState
	if err:=json.Unmarshal(raw,&reloaded);err!=nil { t.Fatalf("unmarshal capacity settings: %v",err) }
	if !reloaded.CapacityAdapters["vision"].RoundRobin || len(reloaded.CapacityAdapters["vision"].Models)!=2 {
		t.Fatalf("capacity policy did not survive state serialization: %#v",reloaded.CapacityAdapters["vision"])
	}
}

func TestSmartStrictLockSelectsOncePerSession(t *testing.T) {
	t.Setenv("FANTOM_DATA_DIR", t.TempDir())
	var decisions atomic.Int64
	var served atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil { t.Errorf("decode request: %v", err); http.Error(w, "bad json", 400); return }
		model, _ := payload["model"].(string)
		w.Header().Set("Content-Type", "application/json")
		switch model {
		case "router-model":
			decisions.Add(1)
			_, _ = io.WriteString(w, "{\"choices\":[{\"message\":{\"content\":\"{\\\"choice\\\":\\\"backup\\\",\\\"confidence\\\":0.96}\"}}],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":20,\"total_tokens\":120}}")
		case "model-b":
			served.Add(1)
			_, _ = io.WriteString(w, "{\"choices\":[{\"message\":{\"content\":\"selected backup\"}}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}")
		default:
			t.Errorf("unexpected upstream model %q", model)
			http.Error(w, "unexpected model", 400)
		}
	}))
	defer upstream.Close()
	cfg := &config.Config{
		DefaultProvider: "main", GatewayAPIKeys: []string{"gateway-secret"}, MaxRequestBytes: 1 << 20,
		DefaultTokenSaver: "off", DefaultCodeMode: "off",
		Providers: []config.Provider{
			{ID: "main", BaseURL: upstream.URL + "/v1", AuthType: "none", Models: []string{"model-a", "model-b"}, ModelPricing: map[string]config.ModelPricing{"model-b": {InputPerMillionUSD:2, OutputPerMillionUSD:4}}, TimeoutSeconds: 5},
			{ID: "decision", BaseURL: upstream.URL + "/v1", AuthType: "none", Models: []string{"router-model"}, ModelPricing: map[string]config.ModelPricing{"router-model": {InputPerMillionUSD:2, OutputPerMillionUSD:4}}, TimeoutSeconds: 5},
		},
		Routes: map[string]config.Route{
			"smart-combo": {Provider: "main", UpstreamModel: "model-a", FallbackModels: []string{"backup"}, Strategy: "smart", SmartDecisionProvider: "decision", SmartDecisionModel: "router-model", SmartPolicy: "strict-lock", SmartConfidence: 0.8, SmartTimeoutSeconds: 2},
			"backup": {Provider: "main", UpstreamModel: "model-b"},
		},
	}
	s := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(s.Close)
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{\"model\":\"smart-combo\",\"messages\":[{\"role\":\"user\",\"content\":\"Analyze this code\"}]}"))
		req.Header.Set("Authorization", "Bearer gateway-secret")
		req.Header.Set("X-Session-ID", "same-session")
		response := httptest.NewRecorder()
		s.ServeHTTP(response, req)
		if response.Code != http.StatusOK { t.Fatalf("smart request %d returned %d: %s", i+1, response.Code, response.Body.String()) }
		if response.Header().Get("X-Fantom-Smart-Selected") != "backup" { t.Fatalf("smart selected header = %q", response.Header().Get("X-Fantom-Smart-Selected")) }
	}
	if decisions.Load() != 1 { t.Fatalf("decision model called %d times, want 1", decisions.Load()) }
	if served.Load() != 2 { t.Fatalf("selected model received %d requests, want 2", served.Load()) }
	rows := s.store.snapshot().Requests
	if len(rows) != 2 { t.Fatalf("smart request history count = %d, want 2", len(rows)) }
	if rows[0].PromptTokens != 110 || rows[0].CompletionTokens != 25 || rows[0].TotalTokens != 135 {
		t.Fatalf("smart decision + completion tokens were not combined: %#v", rows[0])
	}
	if !rows[0].CostKnown || rows[0].CostUSD < 0.00031 || rows[0].CostUSD > 0.00033 {
		t.Fatalf("smart decision + completion cost not combined: %#v", rows[0])
	}
}

func TestSmartSessionCanBeClearedAfterFailover(t *testing.T) {
	s := &Server{}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
	req.Header.Set("X-Session-ID", "same-session")
	s.smartSessions.Store("smart-combo:same-session", smartSessionEntry{Model:"primary", UpdatedAt:time.Now()})
	s.clearSmartSession(req, map[string]json.RawMessage{}, "smart-combo")
	if _, ok := s.smartSessions.Load("smart-combo:same-session"); ok {
		t.Fatal("smart session remained locked after failover")
	}
}

func TestFusionRunsPanelAndJudgeAndAggregatesUsage(t *testing.T) {
	t.Setenv("FANTOM_DATA_DIR", t.TempDir())
	var panelCalls, judgeCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil { t.Errorf("decode request: %v", err); http.Error(w, "bad json", 400); return }
		var model string
		_ = json.Unmarshal(payload["model"], &model)
		if len(payload["tools"]) > 0 { t.Errorf("Fusion forwarded tools to %s", model) }
		if string(payload["stream"]) != "false" { t.Errorf("Fusion panel %s was not non-streaming", model) }
		w.Header().Set("Content-Type", "application/json")
		switch model {
		case "panel-a":
			panelCalls.Add(1)
			_, _ = io.WriteString(w, "{\"choices\":[{\"message\":{\"content\":\"Panel A answer\"}}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}")
		case "panel-b":
			panelCalls.Add(1)
			_, _ = io.WriteString(w, "{\"choices\":[{\"message\":{\"content\":\"Panel B answer\"}}],\"usage\":{\"prompt_tokens\":20,\"completion_tokens\":10,\"total_tokens\":30}}")
		case "judge-model":
			judgeCalls.Add(1)
			_, _ = io.WriteString(w, "{\"choices\":[{\"message\":{\"content\":\"Synthesized final answer\"}}],\"usage\":{\"prompt_tokens\":30,\"completion_tokens\":15,\"total_tokens\":45}}")
		default:
			t.Errorf("unexpected Fusion model %q", model)
			http.Error(w, "unexpected model", 400)
		}
	}))
	defer upstream.Close()
	price := config.ModelPricing{InputPerMillionUSD: 2, OutputPerMillionUSD: 4}
	provider := config.Provider{
		ID: "main", BaseURL: upstream.URL + "/v1", AuthType: "none", Models: []string{"panel-a", "panel-b", "judge-model"}, TimeoutSeconds: 5,
		ModelPricing: map[string]config.ModelPricing{"panel-a": price, "panel-b": price, "judge-model": price},
	}
	cfg := &config.Config{
		DefaultProvider: "main", GatewayAPIKeys: []string{"gateway-secret"}, MaxRequestBytes: 1 << 20,
		DefaultTokenSaver: "off", DefaultCodeMode: "off", Providers: []config.Provider{provider},
		Routes: map[string]config.Route{
			"fusion-combo": {Provider: "main", UpstreamModel: "panel-a", Strategy: "fusion", FusionModels: []string{"panel-a", "panel-b"}, JudgeModel: "judge-model", FusionMinPanel: 2, FusionTimeoutSeconds: 10},
			"panel-a": {Provider: "main", UpstreamModel: "panel-a"},
			"panel-b": {Provider: "main", UpstreamModel: "panel-b"},
			"judge-model": {Provider: "main", UpstreamModel: "judge-model"},
		},
	}
	s := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(s.Close)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{\"model\":\"fusion-combo\",\"messages\":[{\"role\":\"user\",\"content\":\"Compare answers\"}],\"tools\":[{\"type\":\"function\",\"function\":{\"name\":\"do_not_forward\"}}]}"))
	req.Header.Set("Authorization", "Bearer gateway-secret")
	response := httptest.NewRecorder()
	s.ServeHTTP(response, req)
	if response.Code != http.StatusOK { t.Fatalf("Fusion response = %d: %s", response.Code, response.Body.String()) }
	if panelCalls.Load() != 2 || judgeCalls.Load() != 1 { t.Fatalf("panel/judge calls = %d/%d", panelCalls.Load(), judgeCalls.Load()) }
	var result map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil { t.Fatalf("decode Fusion result: %v", err) }
	choices, _ := result["choices"].([]any)
	if len(choices) != 1 { t.Fatalf("Fusion choices = %#v", result["choices"]) }
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	if message["content"] != "Synthesized final answer" { t.Fatalf("Fusion did not use judge: %#v", message) }
	usage, _ := result["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(60) || usage["completion_tokens"] != float64(30) || usage["total_tokens"] != float64(90) {
		t.Fatalf("Fusion usage totals = %#v", usage)
	}
	rows := s.store.snapshot().Requests
	if len(rows) != 1 || rows[0].Engine != "fusion" || !rows[0].CostKnown || rows[0].TotalTokens != 90 { t.Fatalf("Fusion request history = %#v", rows) }
	if s.usageSummary(rows)["priced_requests"] != 1 { t.Fatalf("Fusion price coverage missing: %#v", s.usageSummary(rows)) }
}

func TestFusionSSEEmitsIncrementalChunksAndOptionalUsage(t *testing.T) {
	response:=httptest.NewRecorder()
	textValue:=strings.Repeat("A",120)+"\n"+strings.Repeat("ب",80)
	writeFusionSSE(response,"fusion-model",textValue,40,25,65,true)
	body:=response.Body.String()
	if !strings.HasPrefix(response.Header().Get("Content-Type"),"text/event-stream") {
		t.Fatalf("unexpected Fusion stream content type: %q",response.Header().Get("Content-Type"))
	}
	if strings.Count(body,"data: {")<5 {
		t.Fatalf("Fusion stream was not emitted as incremental events: %s",body)
	}
	if !strings.Contains(body,"\"prompt_tokens\":40") || !strings.Contains(body,"\"completion_tokens\":25") || !strings.Contains(body,"data: [DONE]") {
		t.Fatalf("Fusion stream omitted usage or terminator: %s",body)
	}
	withoutUsage:=httptest.NewRecorder()
	writeFusionSSE(withoutUsage,"fusion-model","answer",1,1,2,false)
	if strings.Contains(withoutUsage.Body.String(),"\"usage\"") {
		t.Fatalf("Fusion emitted usage although include_usage was not requested: %s",withoutUsage.Body.String())
	}
}
func TestFusionSinglePanelDegradesWithoutJudge(t *testing.T) {
	t.Setenv("FANTOM_DATA_DIR", t.TempDir())
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		model, _ := payload["model"].(string)
		w.Header().Set("Content-Type", "application/json")
		if model == "panel-a" {
			_, _ = io.WriteString(w, "{\"choices\":[{\"message\":{\"content\":\"single panel answer\"}}]}")
			return
		}
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
	}))
	defer upstream.Close()
	cfg := &config.Config{
		DefaultProvider: "main", GatewayAPIKeys: []string{"gateway-secret"}, DefaultTokenSaver: "off", DefaultCodeMode: "off",
		Providers: []config.Provider{{ID: "main", BaseURL: upstream.URL + "/v1", AuthType: "none", Models: []string{"panel-a", "panel-b"}, TimeoutSeconds: 3}},
		Routes: map[string]config.Route{
			"fusion-combo": {Provider: "main", UpstreamModel: "panel-a", Strategy: "fusion", FusionModels: []string{"panel-a", "panel-b"}, FusionMinPanel: 2, FusionTimeoutSeconds: 4},
			"panel-a": {Provider: "main", UpstreamModel: "panel-a"},
			"panel-b": {Provider: "main", UpstreamModel: "panel-b"},
		},
	}
	s := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(s.Close)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{\"model\":\"fusion-combo\",\"messages\":[{\"role\":\"user\",\"content\":\"hello\"}]}"))
	req.Header.Set("Authorization", "Bearer gateway-secret")
	res := httptest.NewRecorder()
	s.ServeHTTP(res, req)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), "single panel answer") { t.Fatalf("single-panel degradation returned %d: %s", res.Code, res.Body.String()) }
	if res.Header().Get("X-Fantom-Fusion-Judge") != "single-panel-degraded" { t.Fatalf("unexpected degraded fusion header: %q", res.Header().Get("X-Fantom-Fusion-Judge")) }
}


func TestChatAppliesHeadroomAndCavemanWithoutChangingUserText(t *testing.T) {
	t.Setenv("FANTOM_DATA_DIR", t.TempDir())
	var headroomCalls atomic.Int64
	headroom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headroomCalls.Add(1)
		if r.URL.Path != "/v1/compress" {
			t.Errorf("Headroom path = %q", r.URL.Path)
		}
		var request struct {
			Model string `json:"model"`
			Messages []struct {
				Role string `json:"role"`
				Content string `json:"content"`
				ToolCallID string `json:"tool_call_id"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode Headroom payload: %v", err)
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}
		if request.Model != "test-model" || len(request.Messages) != 3 {
			t.Errorf("unexpected Headroom projection: %#v", request)
		}
		if request.Messages[2].Role != "tool" || request.Messages[2].ToolCallID != "call-1" {
			t.Errorf("Headroom projection lost tool identity: %#v", request.Messages[2])
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"messages": []map[string]string{
				{"role": "user", "content": "Keep this exact user message."},
				{"role": "assistant", "content": ""},
				{"role": "tool", "tool_call_id": "call-1", "content": "3 files changed"},
			},
			"tokens_before": 120, "tokens_after": 70, "tokens_saved": 50,
		})
	}))
	defer headroom.Close()

	var upstreamCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		var payload struct {
			Messages []struct {
				Role string `json:"role"`
				Content string `json:"content"`
				ToolCalls []json.RawMessage `json:"tool_calls"`
			} `json:"messages"`
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode upstream request: %v", err)
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}
		if payload.Model != "test-model" || len(payload.Messages) != 4 {
			t.Errorf("unexpected upstream payload: %#v", payload)
		}
		if payload.Messages[0].Role != "system" ||
			!strings.Contains(payload.Messages[0].Content, "Preserve the user's dominant language") {
			t.Errorf("Caveman instruction did not reach upstream: %#v", payload.Messages)
		}
		if payload.Messages[1].Content != "Keep this exact user message." {
			t.Errorf("Headroom changed a user message without opt-in: %q", payload.Messages[1].Content)
		}
		if payload.Messages[3].Content != "3 files changed" {
			t.Errorf("Headroom-compressed tool text was not forwarded: %q", payload.Messages[3].Content)
		}
		if len(payload.Messages[2].ToolCalls) != 1 {
			t.Errorf("assistant tool-call metadata was lost: %#v", payload.Messages[2])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"OK"}}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`)
	}))
	defer upstream.Close()

	cfg := &config.Config{
		DefaultProvider: "main",
		GatewayAPIKeys: []string{"gateway-secret"},
		DefaultTokenSaver: "safe",
		DefaultCodeMode: "off",
		DefaultCavemanMode: "full",
		HeadroomEnabled: true,
		HeadroomURL: headroom.URL,
		HeadroomTimeoutSeconds: 2,
		HeadroomCompressUserMessages: false,
		Providers: []config.Provider{{
			ID: "main", BaseURL: upstream.URL + "/v1", AuthType: "none",
			Models: []string{"test-model"}, TimeoutSeconds: 5,
		}},
	}
	s := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(s.Close)

	requestBody := `{"model":"test-model","stream":false,"messages":[
		{"role":"user","content":"Keep this exact user message."},
		{"role":"assistant","content":"","tool_calls":[{"id":"call-1","type":"function","function":{"name":"read_file","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"call-1","name":"read_file","content":"file a changed\nfile b changed\nfile c changed"}
	]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(requestBody))
	req.Header.Set("Authorization", "Bearer gateway-secret")
	response := httptest.NewRecorder()
	s.ServeHTTP(response, req)

	if response.Code != http.StatusOK {
		t.Fatalf("chat response = %d: %s", response.Code, response.Body.String())
	}
	if headroomCalls.Load() != 1 || upstreamCalls.Load() != 1 {
		t.Fatalf("Headroom/upstream calls = %d/%d, want 1/1", headroomCalls.Load(), upstreamCalls.Load())
	}
	if response.Header().Get("X-Fantom-Headroom-Status") != "applied" {
		t.Fatalf("Headroom status = %q", response.Header().Get("X-Fantom-Headroom-Status"))
	}
	if response.Header().Get("X-Fantom-Caveman-Mode") != "full" {
		t.Fatalf("Caveman mode header = %q", response.Header().Get("X-Fantom-Caveman-Mode"))
	}
}
