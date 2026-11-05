package server

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mojitex/fantom/internal/config"
	"github.com/mojitex/fantom/internal/optimizer"
	"github.com/mojitex/fantom/internal/routing"
)

type providerRuntime struct {
	config  config.Provider
	client  *http.Client
	nextKey atomic.Uint64
}

func (p *providerRuntime) apiKey() string {
	if len(p.config.APIKeys) == 0 { return "" }
	index := (p.nextKey.Add(1) - 1) % uint64(len(p.config.APIKeys))
	return p.config.APIKeys[index]
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

func (s *Server) Close() {
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
	s.providersMu.Lock(); s.providers=next; s.providersMu.Unlock()
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
		if runtime.config.AuthType=="bearer" && len(runtime.config.APIKeys)==0 {
			for _,model:=range runtime.config.Models { if model!="*" { appendModel(&items,seen,modelItem{ID:model,Object:"model",OwnedBy:runtime.config.ID}) } }
			continue
		}
		modelsURL:=runtime.config.ModelsURL
		if modelsURL=="" { modelsURL=runtime.config.BaseURL+"/models" }
		req,err:=http.NewRequestWithContext(r.Context(),http.MethodGet,modelsURL,nil)
		if err!=nil { continue }
		s.applyProviderHeaders(req,runtime,runtime.apiKey())
		resp,err:=runtime.client.Do(req)
		if err!=nil { s.logger.Warn("provider model catalog request failed","provider",runtime.config.ID,"error",err); continue }
		var remote upstreamModelList
		decodeErr:=json.NewDecoder(io.LimitReader(resp.Body,8<<20)).Decode(&remote)
		_ = resp.Body.Close()
		if resp.StatusCode<200||resp.StatusCode>=300||decodeErr!=nil { s.logger.Warn("provider model catalog returned an error","provider",runtime.config.ID,"status",resp.StatusCode,"decode_error",decodeErr); continue }
		for _,model:=range remote.Data { if model.ID!=""&&modelAllowed(runtime.config.Models,model.ID) { appendModel(&items,seen,modelItem{ID:model.ID,Object:"model",OwnedBy:runtime.config.ID}) } }
	}
	for alias,route:=range state.Routes { if s.providerByID(route.Provider)!=nil { appendModel(&items,seen,modelItem{ID:alias,Object:"model",OwnedBy:route.Provider}) } }
	writeJSON(w,http.StatusOK,map[string]any{"object":"list","data":items})
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
	fallbackAttempts := 0
	for candidateIndex, candidateModel := range candidates {
		if candidateIndex > 0 {
			fallbackAttempts = candidateIndex
		}
		target, resolveErr := routing.Resolve(candidateModel, state.Routes, state.DefaultProvider, providerMap)
		if resolveErr != nil {
			lastErr = resolveErr
			continue
		}
		provider := s.providerByID(target.ProviderID)
		record.Provider = target.ProviderID
		if provider == nil {
			lastErr = fmt.Errorf("provider %q is not available", target.ProviderID)
			continue
		}

		keyAttempts := 1
		if provider.config.AuthType == "bearer" {
			keyAttempts = len(provider.config.APIKeys)
			if keyAttempts == 0 {
				lastErr = fmt.Errorf("provider %q has no configured API key", target.ProviderID)
				continue
			}
			if keyAttempts > 3 {
				keyAttempts = 3
			}
		}

		attemptPayload := make(map[string]json.RawMessage, len(payload))
		for name, value := range payload {
			attemptPayload[name] = value
		}
		modelJSON, _ := json.Marshal(target.UpstreamModel)
		attemptPayload["model"] = modelJSON
		attemptBody, marshalErr := json.Marshal(attemptPayload)
		if marshalErr != nil {
			writeAPIError(w, http.StatusBadRequest, "could not encode routed request", "invalid_request_error")
			return
		}
		attemptBody, optimizationStats := optimizer.Apply(attemptBody, saverMode, codeMode)
		if optimizationStats.ToolResultsChanged > 0 {
			w.Header().Set("X-Fantom-Tool-Results-Optimized", fmt.Sprint(optimizationStats.ToolResultsChanged))
			w.Header().Set("X-Fantom-Bytes-Saved", fmt.Sprint(optimizationStats.SavedBytes()))
			w.Header().Set("X-Fantom-Estimated-Tokens-Saved", fmt.Sprint(optimizationStats.SavedBytes()/4))
		}

		for keyAttempt := 0; keyAttempt < keyAttempts; keyAttempt++ {
			key := ""
			if provider.config.AuthType == "bearer" {
				key = provider.apiKey()
				if key == "" {
					lastErr = fmt.Errorf("provider %q has no configured API key", target.ProviderID)
					break
				}
			}

			upstreamRequest, requestErr := http.NewRequestWithContext(r.Context(), http.MethodPost, provider.config.BaseURL+provider.config.ChatPath, bytes.NewReader(attemptBody))
			if requestErr != nil {
				lastErr = requestErr
				break
			}
			upstreamRequest.Header.Set("Content-Type", "application/json")
			accept := strings.TrimSpace(r.Header.Get("Accept"))
			if accept == "" {
				accept = "application/json"
			}
			upstreamRequest.Header.Set("Accept", accept)
			upstreamRequest.Header.Set("X-Request-ID", r.Header.Get("X-Request-ID"))
			if provider.config.ID == "opencode-console" {
				if session := strings.TrimSpace(r.Header.Get("X-OpenCode-Session")); session != "" && len(session) <= 256 && !strings.ContainsAny(session, "\r\n") {
					upstreamRequest.Header.Set("x-opencode-session", session)
				}
			}
			s.applyProviderHeaders(upstreamRequest, provider, key)
			attempts++
			response, requestErr = provider.client.Do(upstreamRequest)
			if requestErr != nil {
				if r.Context().Err() != nil {
					return
				}
				lastErr = requestErr
				s.logger.Warn("upstream request failed", "provider", provider.config.ID, "model", candidateModel, "error", requestErr, "attempt", attempts, "key_attempt", keyAttempt+1)
				response = nil
				if keyAttempt+1 < keyAttempts {
					continue
				}
				break
			}

			if shouldFallback(response.StatusCode) && keyAttempt+1 < keyAttempts {
				lastErr = fmt.Errorf("provider %s returned HTTP %d", provider.config.ID, response.StatusCode)
				s.logger.Warn("upstream key rejected or throttled; trying another key", "provider", provider.config.ID, "model", candidateModel, "status", response.StatusCode, "key_attempt", keyAttempt+1)
				_ = response.Body.Close()
				response = nil
				continue
			}
			if shouldFallback(response.StatusCode) && candidateIndex < len(candidates)-1 {
				lastErr = fmt.Errorf("provider %s returned HTTP %d", provider.config.ID, response.StatusCode)
				s.logger.Warn("upstream returned fallback-eligible status", "provider", provider.config.ID, "model", candidateModel, "status", response.StatusCode)
				_ = response.Body.Close()
				response = nil
				break
			}

			selectedProvider = provider
			record.Provider = provider.config.ID
			break
		}
		if response != nil {
			break
		}
	}
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
	if attempts > 1 {
		w.Header().Set("X-Fantom-Upstream-Attempts", fmt.Sprint(attempts))
	}
	if fallbackAttempts > 0 {
		w.Header().Set("X-Fantom-Fallback-Attempts", fmt.Sprint(fallbackAttempts))
	}
	copyResponseHeaders(w.Header(), response.Header)
	w.WriteHeader(response.StatusCode)
	usage := s.streamBody(w, response.Body)
	record.PromptTokens = usage.PromptTokens
	record.CompletionTokens = usage.CompletionTokens
	record.TotalTokens = usage.TotalTokens
	if response.StatusCode>=500 {
		providerID:="unknown"; if selectedProvider!=nil { providerID=selectedProvider.config.ID }
		s.logger.Warn("provider returned server error","provider",providerID,"model",input.Model,"status",response.StatusCode)
	}
}

func shouldFallback(status int) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusRequestTimeout, http.StatusConflict, 425, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
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

type tokenUsage struct {
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64
}

func (s *Server) streamBody(w http.ResponseWriter, body io.Reader) tokenUsage {
	controller := http.NewResponseController(w)
	buffer := make([]byte, 32*1024)
	const maxCapturedJSON = 2 << 20
	const maxSSELine = 1 << 20
	captured := make([]byte, 0, 32*1024)
	pendingLine := make([]byte, 0, 4096)
	var usage tokenUsage

	for {
		n, readErr := body.Read(buffer)
		if n > 0 {
			chunk := buffer[:n]
			if len(captured) < maxCapturedJSON {
				remaining := maxCapturedJSON - len(captured)
				if len(chunk) > remaining {
					chunk = chunk[:remaining]
				}
				captured = append(captured, chunk...)
			}

			remaining := buffer[:n]
			for len(remaining) > 0 {
				newline := bytes.IndexByte(remaining, '\n')
				if newline < 0 {
					if len(pendingLine)+len(remaining) <= maxSSELine {
						pendingLine = append(pendingLine, remaining...)
					} else {
						pendingLine = pendingLine[:0]
					}
					break
				}
				if len(pendingLine)+newline <= maxSSELine {
					pendingLine = append(pendingLine, remaining[:newline]...)
					observeSSEUsage(pendingLine, &usage)
				}
				pendingLine = pendingLine[:0]
				remaining = remaining[newline+1:]
			}

			if _, writeErr := w.Write(buffer[:n]); writeErr != nil {
				return usage
			}
			_ = controller.Flush()
		}
		if readErr != nil {
			if readErr != io.EOF {
				s.logger.Warn("error while streaming upstream response", "error", readErr)
			}
			break
		}
	}

	if len(pendingLine) > 0 {
		observeSSEUsage(pendingLine, &usage)
	}
	if capturedUsage := parseResponseUsage(captured); capturedUsage.TotalTokens > 0 || capturedUsage.PromptTokens > 0 || capturedUsage.CompletionTokens > 0 {
		usage = capturedUsage
	}
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	}
	return usage
}

func observeSSEUsage(line []byte, usage *tokenUsage) {
	line = bytes.TrimSpace(line)
	if !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	data := bytes.TrimSpace(line[len("data:"):])
	if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
		return
	}
	next := parseResponseUsage(data)
	if next.TotalTokens > 0 || next.PromptTokens > 0 || next.CompletionTokens > 0 {
		*usage = next
	}
}

func parseResponseUsage(raw []byte) tokenUsage {
	var envelope struct {
		Usage *struct {
			PromptTokens         int64 `json:"prompt_tokens"`
			CompletionTokens     int64 `json:"completion_tokens"`
			TotalTokens          int64 `json:"total_tokens"`
			InputTokens          int64 `json:"input_tokens"`
			OutputTokens         int64 `json:"output_tokens"`
			PromptTokenCount     int64 `json:"promptTokenCount"`
			CandidatesTokenCount int64 `json:"candidatesTokenCount"`
		} `json:"usage"`
	}
	if json.Unmarshal(raw, &envelope) != nil || envelope.Usage == nil {
		return tokenUsage{}
	}
	u := envelope.Usage
	prompt := u.PromptTokens
	if prompt == 0 { prompt = u.InputTokens }
	if prompt == 0 { prompt = u.PromptTokenCount }
	completion := u.CompletionTokens
	if completion == 0 { completion = u.OutputTokens }
	if completion == 0 { completion = u.CandidatesTokenCount }
	total := u.TotalTokens
	if total == 0 { total = prompt + completion }
	return tokenUsage{PromptTokens: prompt, CompletionTokens: completion, TotalTokens: total}
}

func copyResponseHeaders(dst, src http.Header) {
	for name, values := range src {
		switch strings.ToLower(name) {
		case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade", "content-length":
			continue
		}
		for _, value := range values { dst.Add(name, value) }
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
