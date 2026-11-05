package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mojitex/fantom/internal/config"
	"github.com/mojitex/fantom/internal/routing"
)

const (
	protocolModelBodyLimit = 32 << 20
	protocolErrorBodyLimit = 1 << 20
	protocolCandidateLimit = 8
)

type protocolModelRequest struct {
	model        string
	body         []byte
	bodyBuffered bool
	bodyHasModel bool
	geminiPath   bool
	geminiAction string
}

type protocolRouteCandidate struct {
	providerID    string
	upstreamModel string
	internalModel string
}

func (s *Server) serveProtocolCombo(w http.ResponseWriter, r *http.Request) bool {
	if s.core == nil {
		return false
	}
	requestModel, err := readProtocolModelRequest(w, r, s.config.MaxRequestBytes)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return true
	}
	if requestModel.model == "" {
		if requestModel.bodyBuffered {
			restoreProtocolRequestBody(r, requestModel.body)
		}
		return false
	}

	candidates, managed := s.protocolCandidates(r.Context(), requestModel.model)
	if !managed {
		if requestModel.bodyBuffered {
			restoreProtocolRequestBody(r, requestModel.body)
		}
		return false
	}
	if len(candidates) == 0 {
		writeAPIError(w, http.StatusServiceUnavailable,
			"the requested Fantom model route is not available in the embedded protocol runtime",
			"provider_error")
		return true
	}

	started := time.Now()
	recorder := &statusRecorder{ResponseWriter: w}
	record := requestRecord{
		ID: r.Header.Get("X-Request-ID"),
		At: started.UTC(),
		Model: requestModel.model,
		Streaming: requestRequestsStream(r, requestModel.body),
	}
	defer func() {
		if recorder.status == 0 {
			recorder.status = http.StatusOK
		}
		record.Status = recorder.status
		record.LatencyMS = time.Since(started).Milliseconds()
		if s.store != nil {
			s.store.appendRequest(record)
		}
	}()

	for index, candidate := range candidates {
		attempt, buildErr := requestForProtocolCandidate(r, requestModel, candidate.internalModel)
		if buildErr != nil {
			writeAPIError(recorder, http.StatusBadRequest, buildErr.Error(), "invalid_request_error")
			return true
		}

		attemptNumber := index + 1
		gate := newProtocolFallbackWriter(recorder, attemptNumber, index, candidate.providerID)
		s.core.APIProxy().ServeHTTP(gate, attempt)
		if gate.canFallback() && index < len(candidates)-1 {
			continue
		}
		_ = gate.commit()
		record.Provider = candidate.providerID
		record.UpstreamAttempts = attemptNumber
		record.FallbackAttempts = index
		return true
	}

	writeAPIError(recorder, http.StatusBadGateway, "all embedded provider candidates failed", "provider_error")
	record.UpstreamAttempts = len(candidates)
	record.FallbackAttempts = len(candidates) - 1
	return true
}

func (s *Server) protocolCandidates(ctx context.Context, model string) ([]protocolRouteCandidate, bool) {
	state := s.store.snapshot()
	providers := make(map[string]config.Provider, len(state.Providers))
	for _, saved := range state.Providers {
		providers[saved.ID] = saved.runtime()
	}

	if _, isCombo := state.Routes[model]; isCombo {
		names := []string{model}
		visitedNames := make(map[string]struct{})
		visitedTargets := make(map[string]struct{})
		candidates := make([]protocolRouteCandidate, 0, protocolCandidateLimit)

		for len(names) > 0 && len(candidates) < protocolCandidateLimit {
			name := strings.TrimSpace(names[0])
			names = names[1:]
			if name == "" {
				continue
			}
			if _, seen := visitedNames[name]; seen {
				continue
			}
			visitedNames[name] = struct{}{}

			target, err := routing.Resolve(name, state.Routes, state.DefaultProvider, providers)
			if err == nil {
				internal := s.core.ModelForProvider(target.ProviderID, target.UpstreamModel)
				if internal != "" {
					key := target.ProviderID + "\x00" + target.UpstreamModel
					if _, exists := visitedTargets[key]; !exists {
						visitedTargets[key] = struct{}{}
						candidates = append(candidates, protocolRouteCandidate{
							providerID: target.ProviderID,
							upstreamModel: target.UpstreamModel,
							internalModel: internal,
						})
					}
				}
			}
			if route, ok := state.Routes[name]; ok {
				for _, fallback := range route.FallbackModels {
					names = append(names, fallback)
				}
			}
		}
		return candidates, true
	}

	// Direct upstream model IDs can also use any compatible Fantom-managed
	// provider that explicitly exposes the requested model.
	savedProviders := append([]persistedProvider(nil), state.Providers...)
	sort.SliceStable(savedProviders, func(i, j int) bool {
		if savedProviders[i].ID == state.DefaultProvider {
			return true
		}
		if savedProviders[j].ID == state.DefaultProvider {
			return false
		}
		return savedProviders[i].ID < savedProviders[j].ID
	})

	candidates := make([]protocolRouteCandidate, 0, len(savedProviders))
	for _, saved := range savedProviders {
		provider := s.providerByID(saved.ID)
		if provider == nil || !s.providerHasModel(provider, model) {
			continue
		}
		internal := s.core.ModelForProvider(provider.config.ID, model)
		if internal == "" {
			continue
		}
		candidates = append(candidates, protocolRouteCandidate{
			providerID: provider.config.ID,
			upstreamModel: model,
			internalModel: internal,
		})
		if len(candidates) >= protocolCandidateLimit {
			break
		}
	}
	return candidates, len(candidates) > 0
}

func (s *Server) providerHasModel(provider *providerRuntime, model string) bool {
	if provider == nil || model == "" {
		return false
	}
	for _, configured := range provider.config.Models {
		if strings.TrimSpace(configured) == model {
			return true
		}
	}
	provider.catalogMu.Lock()
	defer provider.catalogMu.Unlock()
	for _, discovered := range provider.catalogModels {
		if discovered == model {
			return true
		}
	}
	return false
}

func readProtocolModelRequest(w http.ResponseWriter, r *http.Request, maxBytes int64) (protocolModelRequest, error) {
	result := protocolModelRequest{}
	if strings.HasPrefix(r.URL.Path, "/v1beta/models/") {
		const prefix = "/v1beta/models/"
		escaped := r.URL.EscapedPath()
		rest := strings.TrimPrefix(escaped, prefix)
		colon := strings.LastIndex(rest, ":")
		if colon <= 0 {
			return result, nil
		}
		model, err := url.PathUnescape(rest[:colon])
		if err != nil {
			return result, fmt.Errorf("invalid Gemini model path")
		}
		result.model = model
		result.geminiPath = true
		result.geminiAction = rest[colon:]
	}

	if r.Method != http.MethodPost || (!isProtocolModelBodyPath(r.URL.Path) && !result.geminiPath) || r.Body == nil {
		return result, nil
	}
	if maxBytes <= 0 || maxBytes > protocolModelBodyLimit {
		maxBytes = protocolModelBodyLimit
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBytes+1))
	_ = r.Body.Close()
	if err != nil {
		return result, fmt.Errorf("could not read request body: %w", err)
	}
	if int64(len(body)) > maxBytes {
		return result, fmt.Errorf("request body exceeds the %d MiB limit", maxBytes>>20)
	}
	result.body = body
	result.bodyBuffered = true

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return result, nil
	}
	rawModel, exists := payload["model"]
	if !exists {
		return result, nil
	}
	var model string
	if err := json.Unmarshal(rawModel, &model); err != nil || strings.TrimSpace(model) == "" {
		return result, nil
	}
	result.model = strings.TrimSpace(model)
	result.bodyHasModel = true
	return result, nil
}

func isProtocolModelBodyPath(path string) bool {
	switch strings.TrimSuffix(path, "/") {
	case "/v1/messages", "/v1/responses", "/v1/completions",
		"/v1/embeddings", "/v1/images/generations", "/v1/images/edits",
		"/v1/audio/speech", "/v1beta/interactions":
		return true
	default:
		return false
	}
}

func restoreProtocolRequestBody(r *http.Request, body []byte) {
	if r == nil {
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
}

func requestForProtocolCandidate(original *http.Request, modelRequest protocolModelRequest, internalModel string) (*http.Request, error) {
	clone := original.Clone(original.Context())
	clone.URL = new(url.URL)
	*clone.URL = *original.URL
	clone.Header = original.Header.Clone()

	if modelRequest.bodyBuffered {
		body := modelRequest.body
		if modelRequest.bodyHasModel {
			var payload map[string]json.RawMessage
			if err := json.Unmarshal(body, &payload); err != nil {
				return nil, fmt.Errorf("could not decode request while applying model route: %w", err)
			}
			encodedModel, _ := json.Marshal(internalModel)
			payload["model"] = encodedModel
			updated, err := json.Marshal(payload)
			if err != nil {
				return nil, fmt.Errorf("could not encode routed request: %w", err)
			}
			body = updated
		}
		restoreProtocolRequestBody(clone, body)
		clone.Header.Del("Content-Length")
	}
	if modelRequest.geminiPath {
		const prefix = "/v1beta/models/"
		clone.URL.Path = prefix + internalModel + modelRequest.geminiAction
		clone.URL.RawPath = ""
	}
	return clone, nil
}

func requestRequestsStream(r *http.Request, body []byte) bool {
	if strings.Contains(r.URL.Path, "streamGenerateContent") {
		return true
	}
	if len(body) == 0 {
		return false
	}
	var payload struct {
		Stream bool `json:"stream"`
	}
	return json.Unmarshal(body, &payload) == nil && payload.Stream
}

// protocolFallbackWriter buffers retry-eligible error responses, but commits
// successful responses immediately so SSE and other streams remain incremental.
type protocolFallbackWriter struct {
	dst http.ResponseWriter
	header http.Header
	status int
	committed bool
	overflow bool
	buffer bytes.Buffer
	attempts int
	fallbacks int
	provider string
}

func newProtocolFallbackWriter(dst http.ResponseWriter, attempts, fallbacks int, provider string) *protocolFallbackWriter {
	return &protocolFallbackWriter{
		dst: dst,
		header: make(http.Header),
		attempts: attempts,
		fallbacks: fallbacks,
		provider: provider,
	}
}

func (w *protocolFallbackWriter) Header() http.Header {
	return w.header
}

func (w *protocolFallbackWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	if !shouldFallback(status) {
		w.commitHeader()
	}
}

func (w *protocolFallbackWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if !w.committed && shouldFallback(w.status) && !w.overflow {
		if w.buffer.Len()+len(p) <= protocolErrorBodyLimit {
			return w.buffer.Write(p)
		}
		w.overflow = true
		w.commitHeader()
		if w.buffer.Len() > 0 {
			if _, err := w.dst.Write(w.buffer.Bytes()); err != nil {
				return 0, err
			}
			w.buffer.Reset()
		}
	}
	if !w.committed {
		w.commitHeader()
	}
	return w.dst.Write(p)
}

func (w *protocolFallbackWriter) Flush() {
	_ = w.FlushError()
}

func (w *protocolFallbackWriter) FlushError() error {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if !w.committed && shouldFallback(w.status) && !w.overflow {
		return nil
	}
	if !w.committed {
		w.commitHeader()
	}
	if flusher, ok := w.dst.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}

func (w *protocolFallbackWriter) Unwrap() http.ResponseWriter {
	return w.dst
}

func (w *protocolFallbackWriter) commitHeader() {
	if w.committed {
		return
	}
	if w.status == 0 {
		w.status = http.StatusOK
	}
	for key, values := range w.header {
		w.dst.Header()[key] = append([]string(nil), values...)
	}
	// These diagnostics are gateway-owned and must not be spoofable by upstreams.
	w.dst.Header().Set("X-Fantom-Upstream-Attempts", strconv.Itoa(w.attempts))
	if w.fallbacks > 0 {
		w.dst.Header().Set("X-Fantom-Fallback-Attempts", strconv.Itoa(w.fallbacks))
	}
	w.dst.Header().Set("X-Fantom-Selected-Provider", w.provider)
	w.dst.WriteHeader(w.status)
	w.committed = true
}

func (w *protocolFallbackWriter) canFallback() bool {
	return !w.committed && !w.overflow && shouldFallback(w.status)
}

func (w *protocolFallbackWriter) commit() error {
	if !w.committed {
		w.commitHeader()
	}
	if w.buffer.Len() > 0 {
		_, err := w.dst.Write(w.buffer.Bytes())
		w.buffer.Reset()
		return err
	}
	return nil
}
