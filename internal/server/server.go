package server

import (
	"bytes"
	"context"
	"crypto/subtle"
	"crypto/sha256"
	"errors"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mojitex/fantom/internal/config"
	"github.com/mojitex/fantom/internal/optimizer"
)

type providerRuntime struct {
	config           config.Provider
	client           *http.Client
	nextKey          atomic.Uint64
	keyMu            sync.Mutex
	keyCooldowns     []time.Time
	inflight         chan struct{}
	catalogMu        sync.Mutex
	catalogModels    []string
	catalogFetchedAt time.Time
}

func (p *providerRuntime) tryAcquire() (func(), bool) {
	if p == nil || p.inflight == nil {
		return func() {}, true
	}
	select {
	case p.inflight <- struct{}{}:
		return func() { <-p.inflight }, true
	default:
		return nil, false
	}
}

func (p *providerRuntime) apiKey() string {
	if len(p.config.APIKeys) == 0 { return "" }
	index := (p.nextKey.Add(1) - 1) % uint64(len(p.config.APIKeys))
	return p.config.APIKeys[index]
}

// apiKeysForRequest rotates through credentials that are not currently cooling
// down. An empty result means every configured credential is temporarily
// unavailable and routing should try another candidate rather than hammer them.
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
	seen := make(map[string]struct{}, len(keys))
	for offset := 0; offset < len(keys); offset++ {
		index := (start + offset) % len(keys)
		key := keys[index]
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		if index >= len(p.keyCooldowns) {
			p.keyCooldowns = append(p.keyCooldowns, make([]time.Time, index-len(p.keyCooldowns)+1)...)
		}
		until := p.keyCooldowns[index]
		if until.IsZero() || !now.Before(until) {
			p.keyCooldowns[index] = time.Time{}
			ordered = append(ordered, key)
		}
	}
	// Returning no credentials lets routing advance to another provider instead
	// of probing a key whose Retry-After window has not expired.
	return ordered
}

func (p *providerRuntime) cooldownCredential(key string, status int, retryAfter string) {
	duration := credentialCooldown(status, retryAfter, time.Now())
	if duration <= 0 || key == "" {
		return
	}
	p.keyMu.Lock()
	defer p.keyMu.Unlock()
	until := time.Now().Add(duration)
	for index, configured := range p.config.APIKeys {
		if configured != key {
			continue
		}
		if index >= len(p.keyCooldowns) {
			p.keyCooldowns = append(p.keyCooldowns, make([]time.Time, index-len(p.keyCooldowns)+1)...)
		}
		if until.After(p.keyCooldowns[index]) {
			p.keyCooldowns[index] = until
		}
	}
}

func (p *providerRuntime) credentialCooldownRemaining() time.Duration {
	now := time.Now()
	p.keyMu.Lock()
	defer p.keyMu.Unlock()
	var earliest time.Time
	for _, until := range p.keyCooldowns {
		if until.After(now) && (earliest.IsZero() || until.Before(earliest)) {
			earliest = until
		}
	}
	if earliest.IsZero() {
		return 0
	}
	return earliest.Sub(now)
}

func credentialCooldown(status int, retryAfter string, now time.Time) time.Duration {
	const maxCooldown = time.Hour
	switch status {
	case http.StatusTooManyRequests:
		value := strings.TrimSpace(retryAfter)
		if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
			if seconds >= int(maxCooldown/time.Second) { return maxCooldown }
			return time.Duration(seconds) * time.Second
		}
		if parsed, err := http.ParseTime(value); err == nil {
			duration := parsed.Sub(now)
			if duration > maxCooldown { return maxCooldown }
			if duration < 0 { return 0 }
			return duration
		}
		return 30 * time.Second
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusPaymentRequired:
		return 2 * time.Minute
	default:
		return 0
	}
}

type clientIdentityContextKey struct{}

type clientAdmissionSlot struct {
	slots chan struct{}
	refs  int
}

type clientAdmissionShard struct {
	mu    sync.Mutex
	slots map[[32]byte]*clientAdmissionSlot
}

type Server struct {
	config      *config.Config
	providersMu sync.RWMutex
	providers   map[string]*providerRuntime
	store       *adminStore
	logger      *slog.Logger
	handler       http.Handler
	core          CoreRouter
	requestID     atomic.Uint64
	routeCursors  sync.Map // map[public combo model]*atomic.Uint64
	smartSessions sync.Map // map[combo:session]*smartSessionEntry
	smartSessionSweeps atomic.Uint64
	capacityCursors sync.Map // map[capability]*atomic.Uint64
	coreUsageCancel context.CancelFunc
	coreUsageDone   chan struct{}
	inferenceSlots       chan struct{}
	upstreamSlots        chan struct{}
	maxInflightBodyBytes int64
	inflightBodyBytes    atomic.Int64
	inferenceAdmissions atomic.Uint64
	inferenceRejections atomic.Uint64
	upstreamAdmissions  atomic.Uint64
	upstreamRejections  atomic.Uint64
	providerRejections  atomic.Uint64
	bodyBudgetRejections atomic.Uint64
	clientRejections     atomic.Uint64
	maxConcurrentPerClient int
	clientSlotShards      [64]clientAdmissionShard
}

type modelItem struct {
	ID            string                   `json:"id"`
	Object        string                   `json:"object"`
	OwnedBy       string                   `json:"owned_by"`
	Capabilities  config.ModelCapabilities `json:"capabilities,omitempty"`
	ContextWindow int                      `json:"context_window,omitempty"`
	MaxOutput     int                      `json:"max_output,omitempty"`
	Pricing       *config.ModelPricing     `json:"pricing,omitempty"`
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
		store = &adminStore{path:"./data/fantom-state.json", data:adminState{
			Version:1,DefaultProvider:cfg.DefaultProvider,Routes:cfg.Routes,
			DefaultTokenSaver:cfg.DefaultTokenSaver,DefaultCodeMode:cfg.DefaultCodeMode,
			DefaultCavemanMode:cfg.DefaultCavemanMode,HeadroomEnabled:cfg.HeadroomEnabled,
			HeadroomURL:cfg.HeadroomURL,HeadroomTimeoutSeconds:cfg.HeadroomTimeoutSeconds,
			HeadroomCompressUserMessages:cfg.HeadroomCompressUserMessages,HeadroomSettingsConfigured:true,
		}}
		for _,p := range cfg.Providers { store.data.Providers=append(store.data.Providers,persistProvider(p)) }
	}
	maxConcurrent := cfg.MaxConcurrentRequests
	if maxConcurrent <= 0 {
		maxConcurrent = 256
	}
	maxConcurrentUpstream := cfg.MaxConcurrentUpstreamRequests
	if maxConcurrentUpstream <= 0 {
		maxConcurrentUpstream = 512
	}
	maxConcurrentPerClient := cfg.MaxConcurrentPerClient
	if maxConcurrentPerClient <= 0 {
		maxConcurrentPerClient = 32
	}
	maxInflightBodyBytes := cfg.MaxInflightBodyBytes
	if maxInflightBodyBytes <= 0 {
		maxInflightBodyBytes = 256 << 20
	}
	if maxInflightBodyBytes < cfg.MaxRequestBytes*2 {
		maxInflightBodyBytes = cfg.MaxRequestBytes * 2
	}
	s := &Server{
		config: cfg, providers: make(map[string]*providerRuntime), store: store, logger: logger,
		inferenceSlots: make(chan struct{}, maxConcurrent),
		upstreamSlots: make(chan struct{}, maxConcurrentUpstream),
		maxInflightBodyBytes: maxInflightBodyBytes,
		maxConcurrentPerClient: maxConcurrentPerClient,
	}
	s.reloadProviderRuntime()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /metrics", s.authenticateAdmin(s.handleMetrics))
	mux.HandleFunc("/api/admin/", s.authenticateAdmin(s.handleAdmin))
	mux.HandleFunc("GET /v1/models", s.authenticate(s.handleModels))
	mux.HandleFunc("POST /v1/chat/completions", s.authenticate(s.limitInference(s.handleChatCompletions)))
	// Protocol endpoints not implemented by the compatibility gateway are served
	// by the embedded CLIProxyAPI runtime after Fantom validates the client key.
	mux.Handle("/v1/", s.authenticate(s.limitInference(s.handleCoreAPI)))
	mux.Handle("/v1beta/", s.authenticate(s.limitInference(s.handleCoreAPI)))
	mux.Handle("/openai/v1/", s.authenticate(s.limitInference(s.handleCoreAPI)))
	mux.Handle("/backend-api/codex/", s.authenticate(s.limitInference(s.handleCoreAPI)))
	mux.Handle("/v0/management", s.authenticateAdmin(s.handleCoreManagement))
	mux.Handle("/v0/management/", s.authenticateAdmin(s.handleCoreManagement))
	// v8 management routes power Fantom's native TypeScript account/quota control plane.
	mux.HandleFunc("GET /v8/management/oauth/callback", s.handleCoreCallback)
	mux.HandleFunc("POST /v8/management/oauth/callback", s.handleCoreCallback)
	mux.Handle("/v8/management/", s.authenticateAdmin(s.handleCoreManagement))
	// The static panel must be reachable so it can render its own login form.
	// Its /v0/management API calls remain protected by Fantom and CLIProxyAPI.
	mux.HandleFunc("/management.html", s.handleCoreManagement)
	for _, callback := range []string{
		"/anthropic/callback", "/codex/callback", "/antigravity/callback",
		"/callback", "/devin/callback",
	} {
		mux.HandleFunc(callback, s.handleCoreCallback)
	}
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
	if s.coreUsageCancel != nil {
		s.coreUsageCancel()
	}
	if s.coreUsageDone != nil {
		<-s.coreUsageDone
	}
	if s.core != nil {
		s.core.Close()
	}
	if s.store != nil {
		s.store.Close()
	}
}

// coreUsageEvent is the deliberately small, privacy-safe subset of the
// embedded runtime's usage record that Fantom retains in its unified history.
// Credential secrets, token hashes, client IPs and user-agent data are ignored.
type coreUsageEvent struct {
	Timestamp  time.Time `json:"timestamp"`
	LatencyMS  int64     `json:"latency_ms"`
	TTFTMS     int64     `json:"ttft_ms"`
	Tokens     struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
		TotalTokens  int64 `json:"total_tokens"`
	} `json:"tokens"`
	Failed bool `json:"failed"`
	Stream bool `json:"stream"`
	Fail struct {
		StatusCode int `json:"status_code"`
	} `json:"fail"`
	Provider    string `json:"provider"`
	Model       string `json:"model"`
	Alias       string `json:"alias"`
	AuthIndex   string `json:"auth_index"`
	Endpoint    string `json:"endpoint"`
	RequestID   string `json:"request_id"`
	ExecutionID string `json:"execution_id"`
	Source      string `json:"source"`
}

// collectCoreUsageBatch drains SDK usage events and writes only their safe
// accounting metadata into Fantom's existing persistent request history.
func (s *Server) collectCoreUsageBatch(ctx context.Context) error {
	if s == nil || s.core == nil || s.store == nil {
		return nil
	}
	events, err := s.core.UsageQueue(ctx, 100)
	if err != nil {
		return err
	}
	if len(events) == 0 {
		return nil
	}
	rows := make([]requestRecord, 0, len(events))
	for index, raw := range events {
		var event coreUsageEvent
		if err := json.Unmarshal(raw, &event); err != nil {
			s.logger.Warn("skipping malformed embedded usage event", "error", err)
			continue
		}
		at := event.Timestamp
		if at.IsZero() {
			at = time.Now().UTC()
		} else {
			at = at.UTC()
		}
		status := http.StatusOK
		if event.Failed {
			status = event.Fail.StatusCode
			if status < 400 || status > 599 {
				status = http.StatusBadGateway
			}
		}
		id := strings.TrimSpace(event.ExecutionID)
		if id == "" {
			id = strings.TrimSpace(event.RequestID)
		}
		if id == "" {
			id = fmt.Sprintf("cliproxy-%d-%d", at.UnixNano(), index)
		}
		model := strings.TrimSpace(event.Alias)
		if model == "" {
			model = strings.TrimSpace(event.Model)
		}
		totalTokens := event.Tokens.TotalTokens
		if totalTokens <= 0 {
			totalTokens = event.Tokens.InputTokens + event.Tokens.OutputTokens
		}
		rows = append(rows, requestRecord{
			ID: id, At: at, Provider: strings.TrimSpace(event.Provider), Model: model,
			Status: status, LatencyMS: event.LatencyMS, Streaming: event.Stream,
			PromptTokens: event.Tokens.InputTokens, CompletionTokens: event.Tokens.OutputTokens,
			TotalTokens: totalTokens, Engine: "cliproxy", Account: strings.TrimSpace(event.AuthIndex),
			TTFTMS: event.TTFTMS, Endpoint: strings.TrimSpace(event.Endpoint), Source: strings.TrimSpace(event.Source),
		})
	}
	s.store.appendRequests(rows)
	return nil
}

func (s *Server) runCoreUsageCollector(ctx context.Context, done chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.collectCoreUsageBatch(ctx); err != nil && ctx.Err() == nil {
				s.logger.Debug("embedded usage collection failed", "error", err)
			}
		}
	}
}
func newProviderRuntime(provider config.Provider, maxConcurrent int) *providerRuntime {
	if provider.ChatPath == "" {
		provider.ChatPath = "/chat/completions"
	}
	timeout := provider.TimeoutSeconds
	if timeout <= 0 {
		timeout = 100
	}
	if maxConcurrent <= 0 {
		maxConcurrent = 64
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment, DialContext: dialer.DialContext, ForceAttemptHTTP2: true,
		MaxConnsPerHost: maxConcurrent, MaxIdleConns: maxConcurrent * 2,
		MaxIdleConnsPerHost: maxConcurrent, IdleConnTimeout: 90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second, ExpectContinueTimeout: time.Second,
		ResponseHeaderTimeout: time.Duration(timeout) * time.Second,
	}
	return &providerRuntime{
		config: provider, client: &http.Client{Transport: transport},
		inflight: make(chan struct{}, maxConcurrent),
	}
}

func (s *Server) reloadProviderRuntime() {
	state:=s.store.snapshot()
	next:=make(map[string]*providerRuntime,len(state.Providers))
	maxPerProvider := s.config.MaxConcurrentPerProvider
	if maxPerProvider <= 0 {
		maxPerProvider = 64
	}
	for _, saved := range state.Providers {
		p := saved.runtime()
		next[p.ID] = newProviderRuntime(p, maxPerProvider)
	}
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

// limitInference applies fail-fast admission control to model execution paths.
// A bounded in-flight count protects memory, goroutines and upstream quotas during bursts.
// tryAcquireUpstream bounds total in-flight Fantom-managed upstream calls,
// including fan-out from Fusion and Smart routing. Saturated calls fail fast.
func (s *Server) tryAcquireUpstream() (func(), bool) {
	if s == nil || s.upstreamSlots == nil {
		return func() {}, true
	}
	select {
	case s.upstreamSlots <- struct{}{}:
		s.upstreamAdmissions.Add(1)
		return func() { <-s.upstreamSlots }, true
	default:
		s.upstreamRejections.Add(1)
		return nil, false
	}
}

// tryAcquireBodyBytes reserves a weighted portion of the process-wide request
// body memory budget. Unknown-length uploads reserve the configured per-request
// maximum, preventing chunked uploads from bypassing memory admission.
func (s *Server) tryAcquireBodyBytes(requested int64) (func(), bool) {
	if s == nil || s.maxInflightBodyBytes <= 0 {
		return func() {}, true
	}
	if requested <= 0 {
		requested = 1
	}
	for {
		current := s.inflightBodyBytes.Load()
		if requested > s.maxInflightBodyBytes-current {
			return nil, false
		}
		if s.inflightBodyBytes.CompareAndSwap(current, current+requested) {
			return func() { s.inflightBodyBytes.Add(-requested) }, true
		}
	}
}

// tryAcquireClient provides a bounded, per-bearer-key concurrency budget.
// Entries are removed as soon as their final in-flight request releases its slot,
// so the map cannot grow with the number of clients that have ever connected.
func (s *Server) tryAcquireClient(identity [32]byte) (func(), bool) {
	if s == nil || s.maxConcurrentPerClient <= 0 {
		return func() {}, true
	}
	// Spread admission locks over independent shards so unrelated API keys do
	// not contend on a single global mutex under high request rates.
	shard := &s.clientSlotShards[int(identity[0])%len(s.clientSlotShards)]
	shard.mu.Lock()
	if shard.slots == nil {
		shard.slots = make(map[[32]byte]*clientAdmissionSlot)
	}
	slot := shard.slots[identity]
	if slot == nil {
		slot = &clientAdmissionSlot{slots: make(chan struct{}, s.maxConcurrentPerClient)}
		shard.slots[identity] = slot
	}
	slot.refs++
	select {
	case slot.slots <- struct{}{}:
		shard.mu.Unlock()
		return func() {
			<-slot.slots
			shard.mu.Lock()
			slot.refs--
			if slot.refs == 0 && shard.slots[identity] == slot {
				delete(shard.slots, identity)
			}
			shard.mu.Unlock()
		}, true
	default:
		slot.refs--
		if slot.refs == 0 && shard.slots[identity] == slot {
			delete(shard.slots, identity)
		}
		shard.mu.Unlock()
		return nil, false
	}
}

func (s *Server) limitInference(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		maxBytes := s.config.MaxRequestBytes
		if maxBytes <= 0 {
			maxBytes = 32 << 20
		}
		if r.ContentLength > maxBytes {
			writeAPIError(w, http.StatusRequestEntityTooLarge, "request body exceeds the configured size limit", "invalid_request_error")
			return
		}
		if identity, ok := r.Context().Value(clientIdentityContextKey{}).([32]byte); ok {
			releaseClient, acquired := s.tryAcquireClient(identity)
			if !acquired {
				s.inferenceRejections.Add(1)
				s.clientRejections.Add(1)
				w.Header().Set("Retry-After", "1")
				w.Header().Set("X-Fantom-Admission", "client-limit")
				writeAPIError(w, http.StatusServiceUnavailable, "client is at its configured concurrency limit; retry shortly", "service_unavailable")
				return
			}
			defer releaseClient()
		}
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
		}
		bodyWeight := r.ContentLength
		if bodyWeight < 0 {
			bodyWeight = maxBytes
		}
		if bodyWeight > int64(^uint64(0)>>1)/2 {
			bodyWeight = s.maxInflightBodyBytes + 1
		} else {
			// Reserve twice the wire size for the body buffer and the JSON
			// representation retained while routing/optimizing the request.
			bodyWeight *= 2
		}
		releaseBody, bodyAcquired := s.tryAcquireBodyBytes(bodyWeight)
		if !bodyAcquired {
			s.inferenceRejections.Add(1)
			s.bodyBudgetRejections.Add(1)
			w.Header().Set("Retry-After", "1")
			w.Header().Set("X-Fantom-Admission", "body-budget")
			writeAPIError(w, http.StatusServiceUnavailable, "gateway request-body memory budget is temporarily exhausted", "service_unavailable")
			return
		}
		defer releaseBody()
		if s.inferenceSlots == nil {
			next(w, r)
			return
		}
		select {
		case s.inferenceSlots <- struct{}{}:
			s.inferenceAdmissions.Add(1)
			defer func() { <-s.inferenceSlots }()
			next(w, r)
		default:
			s.inferenceRejections.Add(1)
			w.Header().Set("Retry-After", "1")
			w.Header().Set("X-Fantom-Admission", "rejected")
			writeAPIError(w, http.StatusServiceUnavailable, "gateway is at its configured concurrency limit; retry shortly", "service_unavailable")
		}
	}
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
		identity := sha256.Sum256([]byte(provided))
		ctx := context.WithValue(r.Context(), clientIdentityContextKey{}, identity)
		next(w, r.WithContext(ctx))
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

// handleMetrics exposes a small Prometheus text endpoint. It is protected by
// the administrator bearer key because provider labels and capacity settings
// are operational metadata.
func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = fmt.Fprintf(w, "# HELP fantom_inference_admissions_total Inference requests admitted by the gateway.\n")
	_, _ = fmt.Fprintf(w, "# TYPE fantom_inference_admissions_total counter\nfantom_inference_admissions_total %d\n", s.inferenceAdmissions.Load())
	_, _ = fmt.Fprintf(w, "# HELP fantom_inference_rejections_total Inference requests rejected by admission control.\n")
	_, _ = fmt.Fprintf(w, "# TYPE fantom_inference_rejections_total counter\nfantom_inference_rejections_total %d\n", s.inferenceRejections.Load())
	_, _ = fmt.Fprintf(w, "# HELP fantom_client_limit_rejections_total Requests rejected by per-client concurrency limits.\n")
	_, _ = fmt.Fprintf(w, "# TYPE fantom_client_limit_rejections_total counter\nfantom_client_limit_rejections_total %d\n", s.clientRejections.Load())
	_, _ = fmt.Fprintf(w, "# HELP fantom_body_budget_rejections_total Requests rejected by the weighted body memory budget.\n")
	_, _ = fmt.Fprintf(w, "# TYPE fantom_body_budget_rejections_total counter\nfantom_body_budget_rejections_total %d\n", s.bodyBudgetRejections.Load())
	_, _ = fmt.Fprintf(w, "# HELP fantom_upstream_admissions_total Fantom-managed upstream calls admitted.\n")
	_, _ = fmt.Fprintf(w, "# TYPE fantom_upstream_admissions_total counter\nfantom_upstream_admissions_total %d\n", s.upstreamAdmissions.Load())
	_, _ = fmt.Fprintf(w, "# HELP fantom_upstream_rejections_total Fantom-managed upstream calls rejected by the global limit.\n")
	_, _ = fmt.Fprintf(w, "# TYPE fantom_upstream_rejections_total counter\nfantom_upstream_rejections_total %d\n", s.upstreamRejections.Load())
	_, _ = fmt.Fprintf(w, "# HELP fantom_provider_limit_rejections_total Calls rejected by a per-provider concurrency limit.\n")
	_, _ = fmt.Fprintf(w, "# TYPE fantom_provider_limit_rejections_total counter\nfantom_provider_limit_rejections_total %d\n", s.providerRejections.Load())
	_, _ = fmt.Fprintf(w, "# HELP fantom_inference_in_flight Currently admitted inference requests.\n")
	_, _ = fmt.Fprintf(w, "# TYPE fantom_inference_in_flight gauge\nfantom_inference_in_flight %d\n", len(s.inferenceSlots))
	_, _ = fmt.Fprintf(w, "fantom_inference_limit %d\n", cap(s.inferenceSlots))
	_, _ = fmt.Fprintf(w, "# HELP fantom_upstream_in_flight Currently admitted Fantom-managed upstream calls.\n")
	_, _ = fmt.Fprintf(w, "# TYPE fantom_upstream_in_flight gauge\nfantom_upstream_in_flight %d\n", len(s.upstreamSlots))
	_, _ = fmt.Fprintf(w, "fantom_upstream_limit %d\n", cap(s.upstreamSlots))
	_, _ = fmt.Fprintf(w, "# HELP fantom_request_body_bytes_in_flight Request-body bytes reserved by active inference requests.\n")
	_, _ = fmt.Fprintf(w, "# TYPE fantom_request_body_bytes_in_flight gauge\nfantom_request_body_bytes_in_flight %d\n", s.inflightBodyBytes.Load())
	_, _ = fmt.Fprintf(w, "fantom_request_body_bytes_limit %d\n", s.maxInflightBodyBytes)
	s.providersMu.RLock()
	for id, provider := range s.providers {
		_, _ = fmt.Fprintf(w, "fantom_provider_in_flight{provider=%s} %d\n", strconv.Quote(id), len(provider.inflight))
		_, _ = fmt.Fprintf(w, "fantom_provider_limit{provider=%s} %d\n", strconv.Quote(id), cap(provider.inflight))
	}
	s.providersMu.RUnlock()
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
	state:=s.store.routingSnapshot()
	items:=make([]modelItem,0)
	seen:=make(map[string]struct{})
	providerConfigs := s.providerMap(state)
	for _,saved:=range state.Providers {
		runtime:=s.providerByID(saved.ID)
		if runtime==nil { continue }
		for _,model:=range s.providerModels(r.Context(),runtime) {
			caps := modelCapabilities(runtime.config, model, model)
			item := modelItem{ID:model,Object:"model",OwnedBy:runtime.config.ID,Capabilities:caps,ContextWindow:caps.ContextWindow,MaxOutput:caps.MaxOutput}
			if pricing,ok:=runtime.config.ModelPricing[model];ok { item.Pricing=&pricing }
			appendModel(&items,seen,item)
		}
	}
	for alias,route:=range state.Routes {
		if runtime:=s.providerByID(route.Provider); runtime!=nil {
			caps := s.candidateCapabilities(alias, state, providerConfigs)
			item := modelItem{ID:alias,Object:"model",OwnedBy:route.Provider,Capabilities:caps,ContextWindow:caps.ContextWindow,MaxOutput:caps.MaxOutput}
			if provider,ok:=providerConfigs[route.Provider];ok { if pricing,ok:=provider.ModelPricing[route.UpstreamModel];ok { item.Pricing=&pricing } }
			appendModel(&items,seen,item)
		}
	}
	if s.core != nil {
		coreModels, err := s.core.Models(r.Context())
		if err != nil {
			s.logger.Warn("embedded model catalog unavailable", "error", err)
		} else {
			for _, model := range coreModels {
				ownedBy := model.OwnedBy
				if ownedBy == "" { ownedBy = "cli-proxy-api" }
				caps := modelCapabilities(config.Provider{ID:ownedBy},model.ID,model.ID)
				appendModel(&items, seen, modelItem{ID:model.ID,Object:"model",OwnedBy:ownedBy,Capabilities:caps,ContextWindow:caps.ContextWindow,MaxOutput:caps.MaxOutput})
			}
		}
	}
	writeJSON(w,http.StatusOK,map[string]any{"object":"list","data":items})
}


func (s *Server) providerModels(ctx context.Context, provider *providerRuntime) []string {
	provider.catalogMu.Lock()
	defer provider.catalogMu.Unlock()

	const catalogTTL = 45 * time.Second
	if !provider.catalogFetchedAt.IsZero() && time.Since(provider.catalogFetchedAt)<catalogTTL {
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
			provider.cooldownCredential(key, resp.StatusCode, resp.Header.Get("Retry-After"))
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

	// OAuth-backed and protocol-native models are delegated to the full Go
	// runtime. Explicit Fantom aliases and configured provider models retain
	// their established routing path until migrated into the new provider plane.
	if s.core != nil && s.core.HasModel(r.Context(), input.Model) && !s.legacyModelMatch(r.Context(), input.Model) {
		record.Model = input.Model
		record.Provider = "cli-proxy-api"
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		s.core.APIProxy().ServeHTTP(w, r)
		return
	}

	state:=s.store.routingSnapshot()
	record.Model=input.Model
	record.Streaming=string(payload["stream"])=="true"
	providerMap:=s.providerMap(state)
	candidates:=[]string{input.Model}
	seenCandidates:=map[string]bool{input.Model:true}
	comboStrategy := "fallback"
	primaryRoute, hasCombo := state.Routes[input.Model]
	if hasCombo {
		strategy:=strings.ToLower(strings.TrimSpace(primaryRoute.Strategy))
		if strategy!="" { comboStrategy=strategy }
		for _,fallback:=range primaryRoute.FallbackModels {
			fallback=strings.TrimSpace(fallback)
			if fallback!=""&&!seenCandidates[fallback] { candidates=append(candidates,fallback); seenCandidates[fallback]=true }
		}
	}
	required:=requiredCapabilities(payload)
	if len(required)>0 {
		var names []string
		for name:=range required { names=append(names,name) }
		sort.Strings(names)
		w.Header().Set("X-Fantom-Required-Capabilities",strings.Join(names,","))
	}
	candidates=s.applyCapacityAdapters(candidates,required,state,providerMap)
	candidates=s.reorderByCapabilities(candidates,required,state,providerMap)
	if comboStrategy=="smart" && hasCombo {
		var used bool
		candidates,used=s.smartOrderCandidates(r.Context(),r,payload,input.Model,primaryRoute,candidates,state,providerMap,required,&record)
		if used { w.Header().Set("X-Fantom-Smart-Selected",candidates[0]) }
	}
	if comboStrategy == "round-robin" && len(candidates) > 1 {
		eligible, deferred := s.splitHardCompatibleCandidates(candidates,required,state,providerMap)
		rotationPool := len(eligible)
		if rotationPool == 0 {
			rotationPool = len(candidates)
		} else if len(deferred) > 0 {
			candidates = append(append([]string(nil),eligible...),deferred...)
		}
		if rotationPool > 1 {
			value, _ := s.routeCursors.LoadOrStore(input.Model, &atomic.Uint64{})
			cursor := value.(*atomic.Uint64)
			stickyLimit := primaryRoute.StickyLimit
			offset := roundRobinCandidateOffset(cursor, stickyLimit, rotationPool)
			candidates = rotateCandidatesWithinPrefix(candidates,offset,rotationPool)
		}
	}
	record.RoutingStrategy=comboStrategy

	saverMode:=r.Header.Get("X-Fantom-Token-Saver")
	if strings.TrimSpace(saverMode)=="" { saverMode=state.DefaultTokenSaver }
	codeMode:=r.Header.Get("X-Fantom-Code-Mode")
	if strings.TrimSpace(codeMode)=="" { codeMode=state.DefaultCodeMode }
	cavemanMode:=r.Header.Get("X-Fantom-Caveman-Mode")
	if strings.TrimSpace(cavemanMode)=="" { cavemanMode=state.DefaultCavemanMode }
	saverMode=normalizedMode(saverMode,"safe","off")
	codeMode=normalizedMode(codeMode,"lite","full","ultra")
	cavemanMode=normalizedMode(cavemanMode,"lite","full","ultra")
	w.Header().Set("X-Fantom-Token-Saver",saverMode)
	w.Header().Set("X-Fantom-Code-Mode",codeMode)
	w.Header().Set("X-Fantom-Caveman-Mode",cavemanMode)

	if state.HeadroomEnabled && saverMode!="off" {
		if rawMessages:=payload["messages"];len(rawMessages)>0 {
			timeout:=time.Duration(state.HeadroomTimeoutSeconds)*time.Second
			compressed,stats,headroomErr:=optimizer.CompressWithHeadroom(r.Context(),state.HeadroomURL,input.Model,rawMessages,timeout,state.HeadroomCompressUserMessages)
			if headroomErr==nil {
				payload["messages"]=compressed
				w.Header().Set("X-Fantom-Headroom-Status","applied")
				w.Header().Set("X-Fantom-Headroom-Messages-Changed",fmt.Sprint(stats.MessagesChanged))
				w.Header().Set("X-Fantom-Headroom-Bytes-Saved",fmt.Sprint(stats.OriginalBytes-stats.CompressedBytes))
				if stats.TokensSaved>0 { w.Header().Set("X-Fantom-Headroom-Tokens-Saved",fmt.Sprint(stats.TokensSaved)) }
			} else {
				w.Header().Set("X-Fantom-Headroom-Status","skipped")
				s.logger.Warn("Headroom compression skipped; original request retained","error",headroomErr)
			}
		}
	}

	if comboStrategy=="fusion" && hasCombo {
		fusionRoute:=primaryRoute
		panel:=s.applyCapacityAdapters(primaryRoute.FusionModels,required,state,providerMap)
		eligible,_:=s.splitHardCompatibleCandidates(panel,required,state,providerMap)
		if len(eligible)>0 { panel=eligible }
		panel=s.reorderByCapabilities(panel,required,state,providerMap)
		fusionRoute.FusionModels=panel
		s.handleFusionChat(w,r,payload,input.Model,fusionRoute,state,providerMap,&record,saverMode,codeMode,cavemanMode)
		return
	}

	var response *http.Response
	var responseRelease func()
	var providerSaturated bool
	var lastErr error
	var selectedProvider *providerRuntime
	selectedUpstreamModel:=""
	attempts:=0
	modelFallbacks:=0
	credentialRetries:=0
	cooldownRetrySeconds:=0
	for candidateIndex,candidateModel:=range candidates {
		target,resolveErr:=resolveCandidate(candidateModel,state,providerMap)
		if resolveErr!=nil { lastErr=resolveErr; continue }
		provider:=s.providerByID(target.ProviderID)
		if candidateIndex > 0 { modelFallbacks++ }
		record.Provider=target.ProviderID
		if provider==nil { lastErr=fmt.Errorf("provider %q is not available",target.ProviderID); continue }
		attemptPayload:=make(map[string]json.RawMessage,len(payload))
		for name,value:=range payload { attemptPayload[name]=value }
		modelJSON,_:=json.Marshal(target.UpstreamModel)
		attemptPayload["model"]=modelJSON
		if isCapacityAdapterModel(candidateModel,required,state) {
			caps:=s.candidateCapabilities(candidateModel,state,providerMap)
			trimmed,removed:=stripHistoryForContext(attemptPayload,caps.ContextWindow)
			if removed>0 {
				attemptPayload=trimmed
				w.Header().Set("X-Fantom-Capacity-History-Messages-Dropped",fmt.Sprint(removed))
			}
		}
		attemptBody,marshalErr:=json.Marshal(attemptPayload)
		if marshalErr!=nil { writeAPIError(w,http.StatusBadRequest,"could not encode routed request","invalid_request_error"); return }
		attemptBody,optimizationStats:=optimizer.Apply(attemptBody,saverMode,codeMode,cavemanMode)
		if optimizationStats.ToolResultsChanged>0 {
			w.Header().Set("X-Fantom-Tool-Results-Optimized",fmt.Sprint(optimizationStats.ToolResultsChanged))
			w.Header().Set("X-Fantom-Bytes-Saved",fmt.Sprint(optimizationStats.SavedBytes()))
			w.Header().Set("X-Fantom-Estimated-Tokens-Saved",fmt.Sprint(optimizationStats.SavedBytes()/4))
		}

		keyAttempts:=provider.apiKeysForRequest()
		if provider.config.AuthType=="bearer" && len(keyAttempts)==0 {
			remaining := provider.credentialCooldownRemaining()
			if remaining > 0 {
				seconds := int((remaining + time.Second - 1) / time.Second)
				if cooldownRetrySeconds == 0 || seconds < cooldownRetrySeconds { cooldownRetrySeconds = seconds }
			}
			lastErr=fmt.Errorf("all credentials for provider %q are cooling down",target.ProviderID)
			continue
		}
		responseRelease = nil
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
			releaseUpstream, upstreamAcquired := s.tryAcquireUpstream()
			if !upstreamAcquired {
				providerSaturated = true
				lastErr = errors.New("gateway reached its upstream concurrency limit")
				break
			}
			releaseProvider, acquired := provider.tryAcquire()
			if !acquired {
				releaseUpstream()
				s.providerRejections.Add(1)
				providerSaturated = true
				lastErr = fmt.Errorf("provider %s reached its concurrency limit", provider.config.ID)
				break
			}
			release := func() {
				releaseProvider()
				releaseUpstream()
			}
			attempts++
			response,requestErr=provider.client.Do(upstreamRequest)
			if requestErr!=nil {
				release()
				if r.Context().Err()!=nil { return }
				lastErr=requestErr
				s.logger.Warn("upstream request failed","provider",provider.config.ID,"model",candidateModel,"error",requestErr,"attempt",attempts)
				response=nil
				break
			}
			responseRelease = release
			if shouldRetryCredential(response.StatusCode) {
				provider.cooldownCredential(key, response.StatusCode, response.Header.Get("Retry-After"))
				if keyIndex<len(keyAttempts)-1 {
					lastErr=fmt.Errorf("provider %s credential returned HTTP %d",provider.config.ID,response.StatusCode)
					credentialRetries++
					_ = response.Body.Close()
					release()
					responseRelease = nil
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
			if responseRelease != nil {
				responseRelease()
				responseRelease = nil
			}
			response=nil
			continue
		}
		if response==nil { continue }
		if candidateIndex > 0 && comboStrategy == "smart" {
			s.clearSmartSession(r,payload,input.Model)
		}
		selectedProvider=provider
		record.Provider=provider.config.ID
		selectedUpstreamModel=target.UpstreamModel
		record.UpstreamModel=target.UpstreamModel
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
		if comboStrategy == "smart" {
			s.clearSmartSession(r,payload,input.Model)
			mergeSmartDecisionUsage(&record)
		}
		message:="no configured route could serve this model"
		status:=http.StatusBadGateway
		if cooldownRetrySeconds > 0 {
			status = http.StatusServiceUnavailable
			w.Header().Set("Retry-After", strconv.Itoa(cooldownRetrySeconds))
		} else if providerSaturated {
			status = http.StatusServiceUnavailable
			w.Header().Set("Retry-After", "1")
			message = "all compatible providers are at their concurrency limit"
		}
		if lastErr!=nil && !providerSaturated { message="all route attempts failed: "+lastErr.Error() }
		writeAPIError(w,status,message,"provider_error")
		return
	}
	defer func() {
		_ = response.Body.Close()
		if responseRelease != nil {
			responseRelease()
		}
	}()
	if selectedProvider!=nil {
		w.Header().Set("X-Fantom-Selected-Provider",selectedProvider.config.ID)
	}
	copyResponseHeaders(w.Header(),response.Header)
	w.WriteHeader(response.StatusCode)
	s.streamBodyWithUsage(w,response.Body,&record,response.Header.Get("Content-Type"))
	if selectedProvider!=nil && selectedUpstreamModel!="" {
		if cost,known:=estimateRequestCost(selectedProvider.config,selectedUpstreamModel,record.PromptTokens,record.CompletionTokens);known {
			record.CostUSD=cost
			record.CostKnown=true
		}
	}
	if comboStrategy=="smart" {
		mergeSmartDecisionUsage(&record)
	}
	if response.StatusCode>=500 {
		providerID:="unknown"; if selectedProvider!=nil { providerID=selectedProvider.config.ID }
		s.logger.Warn("provider returned server error","provider",providerID,"model",input.Model,"status",response.StatusCode)
	}
}

// rotateRouteCandidates returns a new ordered candidate slice beginning at offset.
// The original ordering is retained for the ordinary sequential fallback strategy.
func roundRobinCandidateOffset(cursor *atomic.Uint64, stickyLimit, candidateCount int) int {
	if cursor == nil || candidateCount < 2 {
		return 0
	}
	if stickyLimit < 1 {
		stickyLimit = 1
	}
	requestIndex := cursor.Add(1) - 1
	return int((requestIndex / uint64(stickyLimit)) % uint64(candidateCount))
}

func rotateRouteCandidates(candidates []string, offset int) []string {
	if len(candidates) < 2 {
		return candidates
	}
	offset %= len(candidates)
	if offset == 0 {
		return candidates
	}
	ordered := make([]string, 0, len(candidates))
	ordered = append(ordered, candidates[offset:]...)
	ordered = append(ordered, candidates[:offset]...)
	return ordered
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
	isSSE := strings.Contains(strings.ToLower(contentType), "text/event-stream")
	var parser *sseUsageParser
	if isSSE && record != nil {
		parser = &sseUsageParser{record: record}
	}
	// Non-streaming JSON usage is captured only up to a strict per-request cap.
	// SSE is parsed incrementally so response size does not determine memory use.
	const maxJSONUsageCapture = 512 << 10
	var captured []byte
	captureJSON := !isSSE && record != nil
	for {
		n, readErr := body.Read(buffer)
		if n > 0 {
			chunk := buffer[:n]
			if parser != nil {
				parser.feed(chunk)
			} else if captureJSON {
				if len(captured)+n <= maxJSONUsageCapture {
					captured = append(captured, chunk...)
				} else {
					captureJSON = false
					captured = nil
				}
			}
			if _, writeErr := w.Write(chunk); writeErr != nil {
				return
			}
			_ = controller.Flush()
		}
		if readErr != nil {
			if readErr != io.EOF {
				s.logger.Warn("error while streaming upstream response", "error", readErr)
			}
			if parser != nil {
				parser.finish()
			} else if captureJSON && record != nil {
				captureProviderUsage(captured, contentType, record)
			}
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

func applyProviderUsage(record *requestRecord, values usageValues) {
	if record == nil {
		return
	}
	if values.PromptTokens == 0 {
		values.PromptTokens = values.InputTokens
	}
	if values.CompletionTokens == 0 {
		values.CompletionTokens = values.OutputTokens
	}
	if values.PromptTokens > 0 {
		record.PromptTokens = values.PromptTokens
	}
	if values.CompletionTokens > 0 {
		record.CompletionTokens = values.CompletionTokens
	}
	if values.TotalTokens > 0 {
		record.TotalTokens = values.TotalTokens
	} else if record.PromptTokens+record.CompletionTokens > 0 {
		record.TotalTokens = record.PromptTokens + record.CompletionTokens
	}
}

func captureProviderUsage(body []byte, contentType string, record *requestRecord) {
	if strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		parser := &sseUsageParser{record: record}
		parser.feed(body)
		parser.finish()
		return
	}
	var envelope struct {
		Usage   usageValues `json:"usage"`
		Message struct {
			Usage usageValues `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(body, &envelope) != nil {
		return
	}
	applyProviderUsage(record, envelope.Message.Usage)
	applyProviderUsage(record, envelope.Usage)
}

const maxSSEUsageLine = 256 << 10

type sseUsageParser struct {
	pending  []byte
	dropping bool
	record   *requestRecord
}

// feed parses usage from complete SSE data lines without retaining the response.
// Oversized content lines are ignored for accounting but are still streamed to the client.
func (p *sseUsageParser) feed(chunk []byte) {
	for len(chunk) > 0 {
		newline := bytes.IndexByte(chunk, '\n')
		if newline < 0 {
			p.append(chunk)
			return
		}
		p.append(chunk[:newline])
		p.flushLine()
		chunk = chunk[newline+1:]
	}
}

func (p *sseUsageParser) append(part []byte) {
	if p.dropping || len(part) == 0 {
		return
	}
	if len(p.pending)+len(part) > maxSSEUsageLine {
		p.pending = p.pending[:0]
		p.dropping = true
		return
	}
	p.pending = append(p.pending, part...)
}

func (p *sseUsageParser) flushLine() {
	if p.dropping {
		p.dropping = false
		p.pending = p.pending[:0]
		return
	}
	line := bytes.TrimSpace(p.pending)
	if bytes.HasPrefix(line, []byte("data:")) {
		data := bytes.TrimSpace(line[len("data:"):])
		if len(data) > 0 && !bytes.Equal(data, []byte("[DONE]")) {
			var envelope struct {
				Usage usageValues `json:"usage"`
				Message struct {
					Usage usageValues `json:"usage"`
				} `json:"message"`
			}
			if json.Unmarshal(data, &envelope) == nil {
				applyProviderUsage(p.record, envelope.Message.Usage)
				applyProviderUsage(p.record, envelope.Usage)
			}
		}
	}
	p.pending = p.pending[:0]
}

func (p *sseUsageParser) finish() {
	if p != nil && !p.dropping && len(p.pending) > 0 {
		p.flushLine()
	}
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
