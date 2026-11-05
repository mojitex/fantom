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
	config    *config.Config
	providers map[string]*providerRuntime
	logger    *slog.Logger
	requestID atomic.Uint64
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

func New(cfg *config.Config, logger *slog.Logger) http.Handler {
	if logger == nil { logger = slog.Default() }
	s := &Server{config: cfg, providers: make(map[string]*providerRuntime, len(cfg.Providers)), logger: logger}
	for _, provider := range cfg.Providers {
		if provider.ChatPath == "" {
			provider.ChatPath = "/chat/completions"
		}
		dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
		transport := &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: dialer.DialContext,
			ForceAttemptHTTP2: true,
			MaxIdleConns: 256,
			MaxIdleConnsPerHost: 64,
			IdleConnTimeout: 90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
			ExpectContinueTimeout: time.Second,
			ResponseHeaderTimeout: time.Duration(provider.TimeoutSeconds) * time.Second,
		}
		s.providers[provider.ID] = &providerRuntime{config: provider, client: &http.Client{Transport: transport}}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /v1/models", s.authenticate(s.handleModels))
	mux.HandleFunc("POST /v1/chat/completions", s.authenticate(s.handleChatCompletions))
	mux.HandleFunc("/", s.handleDashboard)
	return s.withRequestID(mux)
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

func (s *Server) authenticate(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		value := strings.TrimSpace(r.Header.Get("Authorization"))
		const prefix = "Bearer "
		if !strings.HasPrefix(value, prefix) {
			writeAPIError(w, http.StatusUnauthorized, "missing or invalid bearer token", "authentication_error")
			return
		}
		provided := strings.TrimSpace(strings.TrimPrefix(value, prefix))
		valid := 0
		for _, key := range s.config.GatewayAPIKeys {
			valid |= subtle.ConstantTimeCompare([]byte(provided), []byte(key))
		}
		if valid != 1 {
			writeAPIError(w, http.StatusUnauthorized, "invalid API key", "authentication_error")
			return
		}
		next(w, r)
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
	items := make([]modelItem, 0)
	seen := make(map[string]struct{})
	for _, providerConfig := range s.config.Providers {
		runtime := s.providers[providerConfig.ID]
		if runtime == nil { continue }
		if runtime.config.AuthType == "bearer" && len(runtime.config.APIKeys) == 0 {
			for _, configuredModel := range runtime.config.Models {
				if configuredModel != "*" {
					appendModel(&items, seen, modelItem{ID: configuredModel, Object: "model", OwnedBy: runtime.config.ID})
				}
			}
			continue
		}
		modelsURL := runtime.config.ModelsURL
		if modelsURL == "" {
			modelsURL = runtime.config.BaseURL + "/models"
		}
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, modelsURL, nil)
		if err != nil { continue }
		s.applyProviderHeaders(req, runtime, runtime.apiKey())
		resp, err := runtime.client.Do(req)
		if err != nil {
			s.logger.Warn("provider model catalog request failed", "provider", runtime.config.ID, "error", err)
			continue
		}
		var remote upstreamModelList
		decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&remote)
		_ = resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 || decodeErr != nil {
			s.logger.Warn("provider model catalog returned an error", "provider", runtime.config.ID, "status", resp.StatusCode, "decode_error", decodeErr)
			continue
		}
		for _, model := range remote.Data {
			if model.ID == "" || !modelAllowed(runtime.config.Models, model.ID) { continue }
			appendModel(&items, seen, modelItem{ID: model.ID, Object: "model", OwnedBy: runtime.config.ID})
		}
	}
	for alias, route := range s.config.Routes {
		if s.providers[route.Provider] != nil {
			appendModel(&items, seen, modelItem{ID: alias, Object: "model", OwnedBy: route.Provider})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": items})
}

func appendModel(items *[]modelItem, seen map[string]struct{}, item modelItem) {
	if _, exists := seen[item.ID]; exists { return }
	seen[item.ID] = struct{}{}
	*items = append(*items, item)
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, s.config.MaxRequestBytes)
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

	providerMap := make(map[string]config.Provider, len(s.config.Providers))
	for _, provider := range s.config.Providers { providerMap[provider.ID] = provider }
	target, err := routing.Resolve(input.Model, s.config.Routes, s.config.DefaultProvider, providerMap)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error(), "model_not_found")
		return
	}
	provider := s.providers[target.ProviderID]
	if provider == nil {
		writeAPIError(w, http.StatusBadGateway, "provider is not available", "provider_error")
		return
	}
	key := provider.apiKey()
	if provider.config.AuthType == "bearer" && key == "" {
		writeAPIError(w, http.StatusServiceUnavailable, "provider API key is not configured", "provider_configuration_error")
		return
	}
	if target.UpstreamModel != input.Model {
		modelJSON, _ := json.Marshal(target.UpstreamModel)
		payload["model"] = modelJSON
		body, err = json.Marshal(payload)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, "could not encode routed request", "invalid_request_error")
			return
		}
	}

	saverMode := r.Header.Get("X-Fantom-Token-Saver")
	if strings.TrimSpace(saverMode) == "" {
		saverMode = s.config.DefaultTokenSaver
	}
	codeMode := r.Header.Get("X-Fantom-Code-Mode")
	if strings.TrimSpace(codeMode) == "" {
		codeMode = s.config.DefaultCodeMode
	}
	body, optimizationStats := optimizer.Apply(body, saverMode, codeMode)
	w.Header().Set("X-Fantom-Token-Saver", normalizedMode(saverMode, "safe"))
	w.Header().Set("X-Fantom-Code-Mode", normalizedMode(codeMode, "lite", "full", "ultra"))
	if optimizationStats.ToolResultsChanged > 0 {
		w.Header().Set("X-Fantom-Tool-Results-Optimized", fmt.Sprint(optimizationStats.ToolResultsChanged))
		w.Header().Set("X-Fantom-Bytes-Saved", fmt.Sprint(optimizationStats.SavedBytes()))
		w.Header().Set("X-Fantom-Estimated-Tokens-Saved", fmt.Sprint(optimizationStats.SavedBytes()/4))
	}
	upstreamRequest, err := http.NewRequestWithContext(r.Context(), http.MethodPost, provider.config.BaseURL+provider.config.ChatPath, bytes.NewReader(body))
	if err != nil {
		writeAPIError(w, http.StatusBadGateway, "could not create upstream request", "provider_error")
		return
	}
	upstreamRequest.Header.Set("Content-Type", "application/json")
	accept := strings.TrimSpace(r.Header.Get("Accept"))
	if accept == "" { accept = "application/json" }
	upstreamRequest.Header.Set("Accept", accept)
	upstreamRequest.Header.Set("X-Request-ID", r.Header.Get("X-Request-ID"))
	// OpenCode Console uses this stable value to keep conversation routing and prompt caching consistent.
	if provider.config.ID == "opencode-console" {
		if session := strings.TrimSpace(r.Header.Get("X-OpenCode-Session")); session != "" && len(session) <= 256 && !strings.ContainsAny(session, "\r\n") {
		upstreamRequest.Header.Set("x-opencode-session", session)
		}
	}
	s.applyProviderHeaders(upstreamRequest, provider, key)

	response, err := provider.client.Do(upstreamRequest)
	if err != nil {
		if r.Context().Err() != nil { return }
		s.logger.Warn("upstream request failed", "provider", provider.config.ID, "model", input.Model, "error", err)
		writeAPIError(w, http.StatusBadGateway, "upstream provider connection failed: "+err.Error(), "provider_error")
		return
	}
	defer response.Body.Close()
	copyResponseHeaders(w.Header(), response.Header)
	w.WriteHeader(response.StatusCode)
	s.streamBody(w, response.Body)
	if response.StatusCode >= 500 {
		s.logger.Warn("upstream provider returned server error", "provider", provider.config.ID, "model", input.Model, "status", response.StatusCode)
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

func (s *Server) streamBody(w http.ResponseWriter, body io.Reader) {
	controller := http.NewResponseController(w)
	buffer := make([]byte, 32*1024)
	for {
		n, readErr := body.Read(buffer)
		if n > 0 {
			if _, writeErr := w.Write(buffer[:n]); writeErr != nil { return }
			_ = controller.Flush()
		}
		if readErr != nil {
			if readErr != io.EOF { s.logger.Warn("error while streaming upstream response", "error", readErr) }
			return
		}
	}
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

