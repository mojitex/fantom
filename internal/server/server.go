package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mojitex/fantom/internal/config"
	"github.com/mojitex/fantom/internal/optimizer"
	"github.com/mojitex/fantom/internal/routing"
)

type providerRuntime struct {
	config           config.Provider
	client           *http.Client
	nextKey          atomic.Uint64
	keyMu            sync.Mutex
	keyCooldowns     []time.Time
	catalogMu        sync.Mutex
	catalogModels    []string
	catalogFetchedAt time.Time
}

func (p *providerRuntime) apiKey() string {
	if len(p.config.APIKeys) == 0 { return "" }
	index := (p.nextKey.Add(1) - 1) % uint64(len(p.config.APIKeys))
	return p.config.APIKeys[index]
}

// apiKeysForRequest returns a complete credential rotation beginning at the next
// round-robin position. A failed credential can therefore be skipped without
// permanently pinning a provider to a revoked or rate-limited key.
func (p *providerRuntime) apiKeysForRequest() []string {
	keys := p.config.APIKeys
	if len(keys) == 0 {
		return []string{""}
	}
	start := int((p.nextKey.Add(1) - 1) % uint64(len(keys)))
	now := time.Now()
	p.keyMu.Lock()
	defer p.keyMu.Unlock()

	ordered := make([]string, 0, len(keys))
	earliestIndex := start
	var earliest time.Time
	for offset := 0; offset < len(keys); offset++ {
		index := (start + offset) % len(keys)
		if index >= len(p.keyCooldowns) {
			p.keyCooldowns = append(p.keyCooldowns, make([]time.Time, index-len(p.keyCooldowns)+1)...)
		}
		until := p.keyCooldowns[index]
		if until.IsZero() || !now.Before(until) {
			p.keyCooldowns[index] = time.Time{}
			ordered = append(ordered, keys[index])
			continue
		}
		if earliest.IsZero() || until.Before(earliest) {
			earliest = until
			earliestIndex = index
		}
	}
	if len(ordered) > 0 {
		return ordered
	}

	// If every credential is cooling down, probe only the one whose cooldown
	// expires first instead of hammering every known-throttled credential.
	return []string{keys[earliestIndex]}
}

func (p *providerRuntime) cooldownCredential(key string, status int, retryAfter string) {
	duration := credentialCooldown(status, retryAfter, time.Now())
	if duration <= 0 || key == "" {
		return
	}
	p.keyMu.Lock()
	defer p.keyMu.Unlock()
	for index, configured := range p.config.APIKeys {
		if configured != key {
			continue
		}
		if index >= len(p.keyCooldowns) {
			p.keyCooldowns = append(p.keyCooldowns, make([]time.Time, index-len(p.keyCooldowns)+1)...)
		}
		until := time.Now().Add(duration)
		if until.After(p.keyCooldowns[index]) {
			p.keyCooldowns[index] = until
		}
		return
	}
}

func credentialCooldown(status int, retryAfter string, now time.Time) time.Duration {
	const maxCooldown = time.Hour
	switch status {
	case http.StatusTooManyRequests:
		value := strings.TrimSpace(retryAfter)
		if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
			duration := time.Duration(seconds) * time.Second
			if duration > maxCooldown { return maxCooldown }
			if duration > 0 { return duration }
		}
		if parsed, err := http.ParseTime(value); err == nil {
			duration := parsed.Sub(now)
			if duration > maxCooldown { return maxCooldown }
			if duration > 0 { return duration }
		}
		return 30 * time.Second
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusPaymentRequired:
		return 2 * time.Minute
	default:
		return 0
	}
}

type Server struct {
	config      *config.Config
	providersMu sync.RWMutex
	providers   map[string]*providerRuntime
	store       *adminStore
	logger      *slog.Logger
	handler     http.Handler
	requestID   atomic.Uint64
}

type modelItem struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`
}

type upstreamModelList struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

type modelRequest struct {
	Model string `json:"model"`
}

func New(cfg *config.Config, logger *slog.Logger) *Server {
	if logger == nil { logger = slog.Default() }
	store, err := newAdminStore(cfg, logger)
	if err != nil {
		logger.Error("could not initialize persistent admin state", "error", err)
		store = &adminStore{path:"./data/fantom-state.json", data:adminState{Version:1,DefaultProvider:cfg.DefaultProvider,Routes:cfg.Routes,DefaultTokenSaver:cfg.DefaultTokenSaver,DefaultCodeMode:cfg.DefaultCodeMode}}
		for _,p := range cfg.Providers { store.data.Providers=append(store.data.Providers,persistProvider(p)) }
	}
	s := &Server{config: cfg, providers: make(map[string]*providerRuntime), store:store, logger: logger}
	s.reloadProviderRuntime()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("/api/admin/", s.authenticateAdmin(s.handleAdmin))
	mux.HandleFunc("GET /v1/models", s.authenticate(s.handleModels))
	mux.HandleFunc("POST /v1/chat/completions", s.authenticate(s.handleChatCompletions))
	mux.HandleFunc("/", s.handleDashboard)
	s.handler = s.withRequestID(mux)
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

// Close releases idle upstream connections and flushes the request-history worker.
// It is safe to call after HTTP server shutdown.
func (s *Server) Close() {
	s.providersMu.Lock()
	providers := s.providers
	s.providers = make(map[string]*providerRuntime)
	s.providersMu.Unlock()

	for _, provider := range providers {
		provider.client.CloseIdleConnections()
	}
	if s.store != nil {
		s.store.Close()
	}
}

func newProviderRuntime(provider config.Provider) *providerRuntime {
	if provider.ChatPath=="" { provider.ChatPath="/chat/completions" }
	timeout:=provider.TimeoutSeconds; if timeout<=0 { timeout=100 }
	dialer:=&net.Dialer{Timeout:10*time.Second,KeepAlive:30*time.Second}
	transport:=&http.Transport{Proxy:http.ProxyFromEnvironment,DialContext:dialer.DialContext,ForceAttemptHTTP2:true,MaxIdleConns:256,MaxIdleConnsPerHost:64,IdleConnTimeout:90*time.Second,TLSHandshakeTimeout:10*time.Second,ExpectContinueTimeout:time.Second,ResponseHeaderTimeout:time.Duration(timeout)*time.Second}
	return &providerRuntime{config:provider,client:&http.Client{Transport:transport}}
}

func (s *Server) reloadProviderRuntime() {
	state:=s.store.snapshot()
	next:=make(map[string]*providerRuntime,len(state.Providers))
	for _,saved:=range state.Providers { p:=saved.runtime(); next[p.ID]=newProviderRuntime(p) }
	s.providersMu.Lock()
	old := s.providers
	s.providers = next
	s.providersMu.Unlock()

	// Provider edits replace transports. Close only idle connections; active
	// in-flight requests continue using their existing transport.
	for _, provider := range old {
		provider.client.CloseIdleConnections()
	}
}
func (s *Server) providerByID(id string) *providerRuntime {
	s.providersMu.RLock(); defer s.providersMu.RUnlock()
	return s.providers[id]
}

func (s *Server) withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.Header.Get("X-Request-ID"))
		if len(id) == 0 || len(id) > 128 || strings.ContainsAny(id, "\r\n") {
			id = fmt.Sprintf("%x-%x", time.Now().UnixNano(), s.requestID.Add(1))
		}
		w.Header().Set("X-Request-ID", id)
		r.Header.Set("X-Request-ID", id)
		next.ServeHTTP(w, r)
	})
}

func bearerToken(r *http.Request) (string, bool) {
	value:=strings.TrimSpace(r.Header.Get("Authorization"))
	const prefix="Bearer "
	if !strings.HasPrefix(value,prefix) { return "",false }
	return strings.TrimSpace(strings.TrimPrefix(value,prefix)),true
}

func (s *Server) environmentKeyValid(provided string) bool {
	valid:=0
	for _,key:=range s.config.GatewayAPIKeys {
		if subtle.ConstantTimeCompare([]byte(provided),[]byte(key))==1 { valid=1 }
	}
	return valid==1
}

func (s *Server) authenticate(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter,r *http.Request) {
		provided,ok:=bearerToken(r)
		valid:=ok && s.environmentKeyValid(provided)
		if ok && !valid && s.store!=nil { valid=s.store.keyValid(provided) }
		if !valid {
			writeAPIError(w,http.StatusUnauthorized,"missing or invalid bearer token","authentication_error")
			return
		}
		next(w,r)
	}
}

// Admin operations are intentionally restricted to the environment-level administrator key.
// Generated application keys can call the model API but cannot mutate gateway configuration.
func (s *Server) authenticateAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter,r *http.Request) {
		provided,ok:=bearerToken(r)
		if !ok || !s.environmentKeyValid(provided) {
			writeAPIError(w,http.StatusUnauthorized,"administrator key required","authentication_error")
			return
		}
		next(w,r)
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"service": "fantom-ai-gateway",
		"time": time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeAPIError(w, http.StatusNotFound, "route not found", "not_found")
		return
	}
	root := strings.TrimSpace(s.config.StaticDirectory)
	if root == "" { root = "./dashboard/out" }
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		writeJSON(w, http.StatusOK, map[string]string{"service": "Fantom AI Gateway", "status": "running", "docs": "See the README for API usage."})
		return
	}
	http.FileServer(http.Dir(root)).ServeHTTP(w, r)
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	state:=s.store.snapshot()
	items:=make([]modelItem,0)
	seen:=make(map[string]struct{})
	for _,saved:=range state.Providers {
		runtime:=s.providerByID(saved.ID)
		if runtime==nil { continue }
		for _,model:=range s.providerModels(r.Context(),runtime) {
			appendModel(&items,seen,modelItem{ID:model,Object:"model",OwnedBy:runtime.config.ID})
		}
	}
	for alias,route:=range state.Routes { if s.providerByID(route.Provider)!=nil { appendModel(&items,seen,modelItem{ID:alias,Object:"model",OwnedBy:route.Provider}) } }
	writeJSON(w,http.StatusOK,map[string]any{"object":"list","data":items})
}


func (s *Server) providerModels(ctx context.Context, provider *providerRuntime) []string {
	provider.catalogMu.Lock()
	defer provider.catalogMu.Unlock()

	const catalogTTL = 45 * time.Second
	if len(provider.catalogModels)>0 && time.Since(provider.catalogFetchedAt)<catalogTTL {
		return append([]string(nil),provider.catalogModels...)
	}

	// Keep a usable, explicit allowlist available even when a provider's
	// /models endpoint is unavailable or does not support catalog discovery.
	fallback:=make([]string,0,len(provider.config.Models))
	for _,model:=range provider.config.Models {
		model=strings.TrimSpace(model)
		if model!="" && model!="*" { fallback=append(fallback,model) }
	}
	if provider.config.AuthType=="bearer" && len(provider.config.APIKeys)==0 {
		return fallback
	}

	modelsURL:=provider.config.ModelsURL
	if modelsURL=="" { modelsURL=provider.config.BaseURL+"/models" }
	for _,key:=range provider.apiKeysForRequest() {
		if provider.config.AuthType=="bearer" && key=="" { break }
		req,err:=http.NewRequestWithContext(ctx,http.MethodGet,modelsURL,nil)
		if err!=nil { break }
		s.applyProviderHeaders(req,provider,key)
		resp,err:=provider.client.Do(req)
		if err!=nil {
			s.logger.Warn("provider model catalog request failed","provider",provider.config.ID,"error",err)
			break
		}
		if shouldRetryCredential(resp.StatusCode) {
			_ = resp.Body.Close()
			continue
		}
		if resp.StatusCode<200 || resp.StatusCode>=300 {
			s.logger.Warn("provider model catalog returned an error","provider",provider.config.ID,"status",resp.StatusCode)
			_ = resp.Body.Close()
			break
		}
		var remote upstreamModelList
		decodeErr:=json.NewDecoder(io.LimitReader(resp.Body,8<<20)).Decode(&remote)
		_ = resp.Body.Close()
		if decodeErr!=nil {
			s.logger.Warn("provider model catalog returned invalid JSON","provider",provider.config.ID,"error",decodeErr)
			break
		}
		result:=make([]string,0,len(remote.Data))
		seen:=make(map[string]bool,len(remote.Data))
		for _,model:=range remote.Data {
			if model.ID!="" && modelAllowed(provider.config.Models,model.ID) && !seen[model.ID] {
				seen[model.ID]=true
				result=append(result,model.ID)
			}
		}
		provider.catalogModels=result
		provider.catalogFetchedAt=time.Now()
		return append([]string(nil),result...)
	}

	// Serve the last known-good catalog when a transient provider failure occurs.
	if len(provider.catalogModels)>0 {
		return append([]string(nil),provider.catalogModels...)
	}
	return fallback
}

func appendModel(items *[]modelItem, seen map[string]struct{}, item modelItem) {
	if _, exists := seen[item.ID]; exists { return }
	seen[item.ID] = struct{}{}
	*items = append(*items, item)
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	started:=time.Now()
	recorder:=&statusRecorder{ResponseWriter:w}
	w=recorder
	record:=requestRecord{ID:r.Header.Get("X-Request-ID"),At:started.UTC()}
	defer func(){ if recorder.status==0 { recorder.status=http.StatusOK }; record.Status=recorder.status; record.LatencyMS=time.Since(started).Milliseconds(); if s.store!=nil { s.store.appendRequest(record) } }()
	maxBytes:=s.config.MaxRequestBytes; if maxBytes<=0 { maxBytes=32<<20 }
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPIError(w, http.StatusRequestEntityTooLarge, "request body is too large or unreadable", "invalid_request_error")
		return
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		writeAPIError(w, http.StatusBadRequest, "request body must be valid JSON", "invalid_request_error")
		return
	}
	var input modelRequest
	if raw := payload["model"]; len(raw) == 0 || json.Unmarshal(raw, &input.Model) != nil || strings.TrimSpace(input.Model) == "" {
		writeAPIError(w, http.StatusBadRequest, "model is required", "invalid_request_error")
		return
	}

	state:=s.store.snapshot()
	record.Model=input.Model
	record.Streaming=string(payload["stream"])=="true"
	providerMap:=make(map[string]config.Provider,len(state.Providers))
	for _,saved:=range state.Providers { p:=saved.runtime(); providerMap[p.ID]=p }

	candidates:=[]string{input.Model}
	seenCandidates:=map[string]bool{input.Model:true}
	if primary,ok:=state.Routes[input.Model]; ok {
		for _,fallback:=range primary.FallbackModels {
			fallback=strings.TrimSpace(fallback)
			if fallback!=""&&!seenCandidates[fallback] { candidates=append(candidates,fallback); seenCandidates[fallback]=true }
		}
	}

	saverMode:=r.Header.Get("X-Fantom-Token-Saver")
	if strings.TrimSpace(saverMode)=="" { saverMode=state.DefaultTokenSaver }
	codeMode:=r.Header.Get("X-Fantom-Code-Mode")
	if strings.TrimSpace(codeMode)=="" { codeMode=state.DefaultCodeMode }
	saverMode=normalizedMode(saverMode,"safe","off")
	codeMode=normalizedMode(codeMode,"lite","full","ultra")
	w.Header().Set("X-Fantom-Token-Saver",saverMode)
	w.Header().Set("X-Fantom-Code-Mode",codeMode)

	var response *http.Response
	var lastErr error
	var selectedProvider *providerRuntime
	attempts:=0
	modelFallbacks:=0
	credentialRetries:=0
	for candidateIndex,candidateModel:=range candidates {
		target,resolveErr:=routing.Resolve(candidateModel,state.Routes,state.DefaultProvider,providerMap)
		if resolveErr!=nil { lastErr=resolveErr; continue }
		provider:=s.providerByID(target.ProviderID)
		if candidateIndex > 0 { modelFallbacks++ }
		record.Provider=target.ProviderID
		if provider==nil { lastErr=fmt.Errorf("provider %q is not available",target.ProviderID); continue }
		attemptPayload:=make(map[string]json.RawMessage,len(payload))
		for name,value:=range payload { attemptPayload[name]=value }
		modelJSON,_:=json.Marshal(target.UpstreamModel)
		attemptPayload["model"]=modelJSON
		attemptBody,marshalErr:=json.Marshal(attemptPayload)
		if marshalErr!=nil { writeAPIError(w,http.StatusBadRequest,"could not encode routed request","invalid_request_error"); return }
		attemptBody,optimizationStats:=optimizer.Apply(attemptBody,saverMode,codeMode)
		if optimizationStats.ToolResultsChanged>0 {
			w.Header().Set("X-Fantom-Tool-Results-Optimized",fmt.Sprint(optimizationStats.ToolResultsChanged))
			w.Header().Set("X-Fantom-Bytes-Saved",fmt.Sprint(optimizationStats.SavedBytes()))
			w.Header().Set("X-Fantom-Estimated-Tokens-Saved",fmt.Sprint(optimizationStats.SavedBytes()/4))
		}

		keyAttempts:=provider.apiKeysForRequest()
		for keyIndex,key:=range keyAttempts {
			if provider.config.AuthType=="bearer"&&key=="" {
				lastErr=fmt.Errorf("provider %q has no configured API key",target.ProviderID)
				break
			}
			upstreamRequest,requestErr:=http.NewRequestWithContext(r.Context(),http.MethodPost,provider.config.BaseURL+provider.config.ChatPath,bytes.NewReader(attemptBody))
			if requestErr!=nil { lastErr=requestErr; break }
			upstreamRequest.Header.Set("Content-Type","application/json")
			accept:=strings.TrimSpace(r.Header.Get("Accept")); if accept=="" { accept="application/json" }
			upstreamRequest.Header.Set("Accept",accept)
			upstreamRequest.Header.Set("X-Request-ID",r.Header.Get("X-Request-ID"))
			if provider.config.ID=="opencode-console" {
				if session:=strings.TrimSpace(r.Header.Get("X-OpenCode-Session")); session!=""&&len(session)<=256&&!strings.ContainsAny(session,"\r\n") {
					upstreamRequest.Header.Set("x-opencode-session",session)
				}
			}
			s.applyProviderHeaders(upstreamRequest,provider,key)
			attempts++
			response,requestErr=provider.client.Do(upstreamRequest)
			if requestErr!=nil {
				if r.Context().Err()!=nil { return }
				lastErr=requestErr
				s.logger.Warn("upstream request failed","provider",provider.config.ID,"model",candidateModel,"error",requestErr,"attempt",attempts)
				response=nil
				break
			}
			if shouldRetryCredential(response.StatusCode) {
				provider.cooldownCredential(key, response.StatusCode, response.Header.Get("Retry-After"))
				if keyIndex<len(keyAttempts)-1 {
					lastErr=fmt.Errorf("provider %s credential returned HTTP %d",provider.config.ID,response.StatusCode)
					credentialRetries++
					_ = response.Body.Close()
					response=nil
					continue
				}
			}
			break
		}
		if response!=nil && shouldFallback(response.StatusCode)&&candidateIndex<len(candidates)-1 {
			lastErr=fmt.Errorf("provider %s returned HTTP %d",provider.config.ID,response.StatusCode)
			s.logger.Warn("upstream returned fallback-eligible status","provider",provider.config.ID,"model",candidateModel,"status",response.StatusCode)
			_ = response.Body.Close()
			response=nil
			continue
		}
		if response==nil { continue }
		selectedProvider=provider
		record.Provider=provider.config.ID
		break
	}
	if attempts > 0 {
		w.Header().Set("X-Fantom-Upstream-Attempts", fmt.Sprint(attempts))
	}
	if modelFallbacks > 0 {
		w.Header().Set("X-Fantom-Fallback-Attempts", fmt.Sprint(modelFallbacks))
	}
	if credentialRetries > 0 {
		w.Header().Set("X-Fantom-Credential-Retries", fmt.Sprint(credentialRetries))
	}
	record.UpstreamAttempts = attempts
	record.CredentialRetries = credentialRetries
	record.FallbackAttempts = modelFallbacks

	if response==nil {
		message:="no configured route could serve this model"
		status:=http.StatusBadGateway
		if lastErr!=nil { message="all route attempts failed: "+lastErr.Error() }
		writeAPIError(w,status,message,"provider_error")
		return
	}
	defer response.Body.Close()
	if selectedProvider!=nil {
		w.Header().Set("X-Fantom-Selected-Provider",selectedProvider.config.ID)
	}
	copyResponseHeaders(w.Header(),response.Header)
	w.WriteHeader(response.StatusCode)
	s.streamBodyWithUsage(w,response.Body,&record,response.Header.Get("Content-Type"))
	if response.StatusCode>=500 {
		providerID:="unknown"; if selectedProvider!=nil { providerID=selectedProvider.config.ID }
		s.logger.Warn("provider returned server error","provider",providerID,"model",input.Model,"status",response.StatusCode)
	}
}

func shouldRetryCredential(status int) bool {
	// These responses are commonly scoped to an individual credential.
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusPaymentRequired, http.StatusTooManyRequests:
		return true
	default:
		return false
	}
}

func shouldFallback(status int) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusPaymentRequired, http.StatusRequestTimeout, http.StatusConflict, 425, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func normalizedMode(value string, allowed ...string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	for _, candidate := range allowed {
		if value == candidate {
			return value
		}
	}
	return "off"
}

func (s *Server) applyProviderHeaders(req *http.Request, provider *providerRuntime, key string) {
	if provider.config.AuthType == "bearer" && key != "" { req.Header.Set("Authorization", "Bearer "+key) }
	for name, value := range provider.config.Headers {
		if strings.EqualFold(name, "Authorization") { continue }
		req.Header.Set(name, value)
	}
	req.Header.Set("User-Agent", "Fantom-AI-Gateway/0.1")
}

func (s *Server) streamBodyWithUsage(w http.ResponseWriter, body io.Reader, record *requestRecord, contentType string) {
	controller := http.NewResponseController(w)
	buffer := make([]byte, 32*1024)
	const maxUsageCapture = 8 << 20
	var captured bytes.Buffer
	captureEnabled := true
	for {
		n, readErr := body.Read(buffer)
		if n > 0 {
			if captureEnabled {
				if captured.Len()+n <= maxUsageCapture {
					_, _ = captured.Write(buffer[:n])
				} else {
					// Never let observability buffering grow with an unbounded model stream.
					captureEnabled = false
					captured.Reset()
				}
			}
			if _, writeErr := w.Write(buffer[:n]); writeErr != nil { return }
			_ = controller.Flush()
		}
		if readErr != nil {
			if readErr != io.EOF { s.logger.Warn("error while streaming upstream response", "error", readErr) }
			if captureEnabled && record != nil { captureProviderUsage(captured.Bytes(),contentType,record) }
			return
		}
	}
}

type usageValues struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
}

func captureProviderUsage(body []byte, contentType string, record *requestRecord) {
	apply := func(values usageValues) {
		if values.PromptTokens == 0 { values.PromptTokens = values.InputTokens }
		if values.CompletionTokens == 0 { values.CompletionTokens = values.OutputTokens }
		if values.PromptTokens > 0 { record.PromptTokens = values.PromptTokens }
		if values.CompletionTokens > 0 { record.CompletionTokens = values.CompletionTokens }
		if values.TotalTokens > 0 { record.TotalTokens = values.TotalTokens } else if record.PromptTokens+record.CompletionTokens > 0 {
			record.TotalTokens = record.PromptTokens+record.CompletionTokens
		}
	}
	if strings.Contains(strings.ToLower(contentType),"text/event-stream") {
		scanner := bufio.NewScanner(bytes.NewReader(body))
		scanner.Buffer(make([]byte, 64*1024), 1<<20)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if !strings.HasPrefix(line,"data:") { continue }
			data := strings.TrimSpace(strings.TrimPrefix(line,"data:"))
			if data == "" || data == "[DONE]" { continue }
			var envelope struct {
				Usage usageValues `json:"usage"`
				Message struct { Usage usageValues `json:"usage"` } `json:"message"`
			}
			if json.Unmarshal([]byte(data),&envelope)==nil {
				apply(envelope.Message.Usage)
				apply(envelope.Usage)
			}
		}
		return
	}
	var envelope struct { Usage usageValues `json:"usage"` }
	if json.Unmarshal(body,&envelope)==nil { apply(envelope.Usage) }
}

func copyResponseHeaders(dst, src http.Header) {
	for name, values := range src {
		lowerName := strings.ToLower(name)
		// These headers belong to the gateway. Never let an upstream inject
		// duplicate or misleading routing, retry, or request-correlation data.
		if lowerName == "x-request-id" || strings.HasPrefix(lowerName, "x-fantom-") {
			continue
		}
		switch lowerName {
		case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade", "content-length":
			continue
		}
		for _, value := range values {
			dst.Add(name, value)
		}
	}
}

func modelAllowed(allowed []string, model string) bool {
	for _, candidate := range allowed {
		if candidate == "*" || candidate == model { return true }
	}
	return false
}

func writeAPIError(w http.ResponseWriter, status int, message, errorType string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"message": message, "type": errorType}})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}



type statusRecorder struct { http.ResponseWriter; status int }
func (w *statusRecorder) WriteHeader(code int) { if w.status!=0 { return }; w.status=code; w.ResponseWriter.WriteHeader(code) }
func (w *statusRecorder) Write(p []byte) (int,error) { if w.status==0 { w.WriteHeader(http.StatusOK) }; return w.ResponseWriter.Write(p) }
func (w *statusRecorder) Flush() { if w.status==0 { w.WriteHeader(http.StatusOK) }; if f,ok:=w.ResponseWriter.(http.Flusher); ok { f.Flush() } }
func (w *statusRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }
