package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mojitex/fantom/internal/config"
)

type persistedProvider struct {
	ID             string            `json:"id"`
	BaseURL        string            `json:"base_url"`
	ChatPath       string            `json:"chat_path"`
	ModelsURL      string            `json:"models_url,omitempty"`
	AuthType       string            `json:"auth_type"`
	Models         []string          `json:"models"`
	TimeoutSeconds int               `json:"timeout_seconds"`
	Headers           map[string]string                 `json:"headers,omitempty"`
	ModelCapabilities map[string]config.ModelCapabilities `json:"model_capabilities,omitempty"`
	ModelPricing      map[string]config.ModelPricing      `json:"model_pricing,omitempty"`
	APIKeys           []string                           `json:"api_keys,omitempty"`
}

func cloneModelCapabilities(source map[string]config.ModelCapabilities) map[string]config.ModelCapabilities {
	if len(source) == 0 { return nil }
	out := make(map[string]config.ModelCapabilities, len(source))
	for key, value := range source { out[key] = value }
	return out
}
func cloneModelPricing(source map[string]config.ModelPricing) map[string]config.ModelPricing {
	if len(source) == 0 { return nil }
	out := make(map[string]config.ModelPricing, len(source))
	for key, value := range source { out[key] = value }
	return out
}
func persistProvider(p config.Provider) persistedProvider {
	return persistedProvider{ID:p.ID, BaseURL:p.BaseURL, ChatPath:p.ChatPath, ModelsURL:p.ModelsURL, AuthType:p.AuthType, Models:append([]string(nil),p.Models...), TimeoutSeconds:p.TimeoutSeconds, Headers:p.Headers, ModelCapabilities:cloneModelCapabilities(p.ModelCapabilities), ModelPricing:cloneModelPricing(p.ModelPricing), APIKeys:append([]string(nil),p.APIKeys...)}
}
func (p persistedProvider) runtime() config.Provider {
	return config.Provider{ID:p.ID, BaseURL:p.BaseURL, ChatPath:p.ChatPath, ModelsURL:p.ModelsURL, AuthType:p.AuthType, Models:append([]string(nil),p.Models...), TimeoutSeconds:p.TimeoutSeconds, Headers:p.Headers, ModelCapabilities:cloneModelCapabilities(p.ModelCapabilities), ModelPricing:cloneModelPricing(p.ModelPricing), APIKeys:append([]string(nil),p.APIKeys...)}
}

type storedClientKey struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Prefix    string    `json:"prefix"`
	Hash      string    `json:"hash"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
}

type requestRecord struct {
	ID               string    `json:"id"`
	At               time.Time `json:"at"`
	Provider         string    `json:"provider"`
	Model            string    `json:"model"`
	UpstreamModel    string    `json:"upstream_model,omitempty"`
	Status           int       `json:"status"`
	CostUSD          float64   `json:"cost_usd,omitempty"`
	CostKnown        bool      `json:"cost_known,omitempty"`
	RoutingStrategy  string    `json:"routing_strategy,omitempty"`
	LatencyMS        int64     `json:"latency_ms"`
	Streaming        bool      `json:"streaming"`
	PromptTokens     int64     `json:"prompt_tokens,omitempty"`
	CompletionTokens int64     `json:"completion_tokens,omitempty"`
	TotalTokens      int64     `json:"total_tokens,omitempty"`
	UpstreamAttempts int       `json:"upstream_attempts,omitempty"`
	CredentialRetries int      `json:"credential_retries,omitempty"`
	FallbackAttempts int       `json:"fallback_attempts,omitempty"`
	Engine           string    `json:"engine,omitempty"`
	Account           string    `json:"account,omitempty"`
	TTFTMS            int64     `json:"ttft_ms,omitempty"`
	Endpoint          string    `json:"endpoint,omitempty"`
	Source            string    `json:"source,omitempty"`
}

type adminState struct {
	Version            int                     `json:"version"`
	DefaultProvider    string                  `json:"default_provider"`
	Providers          []persistedProvider     `json:"providers"`
	Routes             map[string]config.Route `json:"routes"`
	CapacityAdapters   map[string][]string     `json:"capacity_adapters,omitempty"`
	DefaultTokenSaver  string                  `json:"default_token_saver"`
	DefaultCodeMode    string                  `json:"default_code_mode"`
	ClientKeys         []storedClientKey       `json:"client_keys"`
	Requests           []requestRecord         `json:"requests"`
}

type adminStore struct {
	mu        sync.RWMutex
	path      string
	data      adminState
	dirty     bool
	flushWake chan struct{}
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	logger    *slog.Logger
}

func newAdminStore(cfg *config.Config, logger *slog.Logger) (*adminStore, error) {
	dir := strings.TrimSpace(os.Getenv("FANTOM_DATA_DIR"))
	if dir == "" { dir = "./data" }
	path := filepath.Join(dir, "fantom-state.json")
	store := &adminStore{path:path, logger:logger}
	store.data = adminState{Version:1, DefaultProvider:cfg.DefaultProvider, Routes:cfg.Routes, DefaultTokenSaver:cfg.DefaultTokenSaver, DefaultCodeMode:cfg.DefaultCodeMode}
	for _, p := range cfg.Providers { store.data.Providers = append(store.data.Providers, persistProvider(p)) }
	if store.data.Routes == nil { store.data.Routes = map[string]config.Route{} }
	if store.data.CapacityAdapters == nil { store.data.CapacityAdapters = map[string][]string{} }
	raw, err := os.ReadFile(path)
	if err == nil {
		var loaded adminState
		if decodeErr := json.Unmarshal(raw, &loaded); decodeErr != nil { return nil, fmt.Errorf("decode Fantom state: %w", decodeErr) }
		if loaded.Version != 1 { return nil, fmt.Errorf("unsupported Fantom state version %d", loaded.Version) }
		if loaded.Routes == nil { loaded.Routes = map[string]config.Route{} }
		if loaded.CapacityAdapters == nil { loaded.CapacityAdapters = map[string][]string{} }
		if loaded.DefaultTokenSaver == "" { loaded.DefaultTokenSaver = cfg.DefaultTokenSaver }
		if loaded.DefaultCodeMode == "" { loaded.DefaultCodeMode = cfg.DefaultCodeMode }
		if len(loaded.Providers) == 0 { loaded.Providers = store.data.Providers }
		if loaded.DefaultProvider == "" { loaded.DefaultProvider = cfg.DefaultProvider }
		store.data = loaded
	} else if !errors.Is(err, os.ErrNotExist) { return nil, fmt.Errorf("read Fantom state: %w", err) }
	if err := store.saveLocked(); err != nil { return nil, err }
	store.flushWake = make(chan struct{}, 1)
	store.stop = make(chan struct{})
	store.done = make(chan struct{})
	go store.runPersistence()
	return store, nil
}

func (d *adminStore) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(d.path), 0700); err != nil { return fmt.Errorf("create data directory: %w",err) }
	raw, err := json.MarshalIndent(d.data, "", "  ")
	if err != nil { return err }
	tmp := d.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0600); err != nil { return fmt.Errorf("write state: %w",err) }
	if err := os.Rename(tmp, d.path); err != nil { _ = os.Remove(tmp); return fmt.Errorf("commit state: %w",err) }
	d.dirty = false
	return nil
}

func (d *adminStore) snapshot() adminState {
	d.mu.RLock(); defer d.mu.RUnlock()
	out := d.data
	out.Providers = append([]persistedProvider(nil), d.data.Providers...)
	out.Routes = make(map[string]config.Route,len(d.data.Routes)); for k,v := range d.data.Routes { out.Routes[k]=v }
	out.CapacityAdapters = make(map[string][]string, len(d.data.CapacityAdapters))
	for capability, models := range d.data.CapacityAdapters { out.CapacityAdapters[capability] = append([]string(nil), models...) }
	for i := range out.Providers {
		out.Providers[i].ModelCapabilities = cloneModelCapabilities(out.Providers[i].ModelCapabilities)
		out.Providers[i].ModelPricing = cloneModelPricing(out.Providers[i].ModelPricing)
	}
	out.ClientKeys = append([]storedClientKey(nil),d.data.ClientKeys...)
	out.Requests = append([]requestRecord(nil),d.data.Requests...)
	return out
}

func (d *adminStore) replaceProvider(p config.Provider) error {
	d.mu.Lock(); defer d.mu.Unlock()
	found := false
	for i, old := range d.data.Providers { if old.ID == p.ID { d.data.Providers[i]=persistProvider(p); found=true; break } }
	if !found { d.data.Providers=append(d.data.Providers,persistProvider(p)) }
	if d.data.DefaultProvider == "" { d.data.DefaultProvider=p.ID }
	return d.saveLocked()
}
func (d *adminStore) deleteProvider(id string) error {
	d.mu.Lock(); defer d.mu.Unlock()
	next:=make([]persistedProvider,0,len(d.data.Providers))
	for _,p:=range d.data.Providers { if p.ID!=id { next=append(next,p) } }
	if len(next)==len(d.data.Providers) { return os.ErrNotExist }
	if len(next)==0 { return errors.New("at least one provider must remain configured") }
	d.data.Providers=next
	for alias,route:=range d.data.Routes { if route.Provider==id { delete(d.data.Routes,alias) } }
	if d.data.DefaultProvider==id { d.data.DefaultProvider=next[0].ID }
	return d.saveLocked()
}
func (d *adminStore) setRoutes(routes map[string]config.Route) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	providers := map[string]bool{}
	for _, p := range d.data.Providers { providers[p.ID] = true }

	for alias, route := range routes {
		alias = strings.TrimSpace(alias)
		if alias == "" { return errors.New("model alias cannot be empty") }
		route.Provider = strings.TrimSpace(route.Provider)
		route.UpstreamModel = strings.TrimSpace(route.UpstreamModel)
		if !providers[route.Provider] { return fmt.Errorf("route %q references unknown provider %q", alias, route.Provider) }
		if route.UpstreamModel == "" { return fmt.Errorf("route %q needs an upstream model", alias) }

		strategy := strings.ToLower(strings.TrimSpace(route.Strategy))
		if strategy == "" { strategy = "fallback" }
		switch strategy {
		case "fallback", "round-robin", "smart", "fusion":
		default:
			return fmt.Errorf("route %q has unsupported strategy %q", alias, strategy)
		}
		route.Strategy = strategy

		if route.StickyLimit < 0 || route.StickyLimit > 10000 {
			return fmt.Errorf("route %q sticky limit must be between 0 and 10000", alias)
		}
		if strategy == "round-robin" && route.StickyLimit == 0 { route.StickyLimit = 1 }
		if strategy != "round-robin" { route.StickyLimit = 0 }

		cleanFallbacks := make([]string, 0, len(route.FallbackModels))
		seen := map[string]bool{alias: true}
		for _, fallback := range route.FallbackModels {
			fallback = strings.TrimSpace(fallback)
			if fallback == "" { return fmt.Errorf("route %q contains an empty fallback model", alias) }
			if seen[fallback] { return fmt.Errorf("route %q contains duplicate or recursive candidate %q", alias, fallback) }
			seen[fallback] = true
			cleanFallbacks = append(cleanFallbacks, fallback)
		}
		route.FallbackModels = cleanFallbacks

		if strategy == "smart" {
			if len(route.FallbackModels) == 0 {
				return fmt.Errorf("route %q smart routing requires at least one alternative candidate model", alias)
			}
			route.SmartDecisionProvider = strings.TrimSpace(route.SmartDecisionProvider)
			route.SmartDecisionModel = strings.TrimSpace(route.SmartDecisionModel)
			if !providers[route.SmartDecisionProvider] { return fmt.Errorf("route %q smart decision provider is unknown", alias) }
			if route.SmartDecisionModel == "" { return fmt.Errorf("route %q needs a smart decision model", alias) }
			policy := strings.ToLower(strings.TrimSpace(route.SmartPolicy))
			if policy == "" { policy = "smart-switch" }
			if policy != "smart-switch" && policy != "strict-lock" && policy != "per-turn" {
				return fmt.Errorf("route %q has unsupported smart policy %q", alias, policy)
			}
			route.SmartPolicy = policy
			if route.SmartConfidence == 0 { route.SmartConfidence = 0.8 }
			if route.SmartConfidence < 0.5 || route.SmartConfidence > 0.95 {
				return fmt.Errorf("route %q confidence threshold must be between 0.50 and 0.95", alias)
			}
			if route.SmartTimeoutSeconds <= 0 { route.SmartTimeoutSeconds = 8 }
			if route.SmartTimeoutSeconds > 30 { return fmt.Errorf("route %q smart timeout cannot exceed 30 seconds", alias) }
		} else {
			route.SmartDecisionProvider = ""
			route.SmartDecisionModel = ""
			route.SmartPolicy = ""
			route.SmartConfidence = 0
			route.SmartTimeoutSeconds = 0
		}

		if strategy == "fusion" {
			if len(route.FusionModels) == 0 { route.FusionModels = append([]string(nil), route.FallbackModels...) }
			if len(route.FusionModels) < 2 || len(route.FusionModels) > 8 {
				return fmt.Errorf("route %q fusion requires between 2 and 8 panel models", alias)
			}
			cleanPanel := make([]string, 0, len(route.FusionModels))
			panelSeen := map[string]bool{}
			for _, model := range route.FusionModels {
				model = strings.TrimSpace(model)
				if model == "" || panelSeen[model] || model == alias {
					return fmt.Errorf("route %q contains an empty, duplicate or recursive fusion model", alias)
				}
				panelSeen[model] = true
				cleanPanel = append(cleanPanel, model)
			}
			route.FusionModels = cleanPanel
			route.JudgeModel = strings.TrimSpace(route.JudgeModel)
			if route.JudgeModel == "" { route.JudgeModel = cleanPanel[0] }
			if route.FusionMinPanel <= 0 { route.FusionMinPanel = 2 }
			if route.FusionMinPanel > len(cleanPanel) { return fmt.Errorf("route %q fusion quorum exceeds panel size", alias) }
			if route.FusionTimeoutSeconds <= 0 { route.FusionTimeoutSeconds = 45 }
			if route.FusionTimeoutSeconds > 180 { return fmt.Errorf("route %q fusion timeout cannot exceed 180 seconds", alias) }
		} else {
			route.FusionModels = nil
			route.JudgeModel = ""
			route.FusionMinPanel = 0
			route.FusionTimeoutSeconds = 0
		}
		routes[alias] = route
	}
	d.data.Routes = routes
	return d.saveLocked()
}

func (d *adminStore) setCapacityAdapters(adapters map[string][]string) error {
	allowed := map[string]bool{"vision":true, "pdf":true, "audio_input":true, "video_input":true}
	clean := make(map[string][]string, len(adapters))
	for capability, models := range adapters {
		capability = strings.ToLower(strings.TrimSpace(capability))
		if !allowed[capability] { return fmt.Errorf("unsupported capacity adapter capability %q", capability) }
		seen := map[string]bool{}
		for _, model := range models {
			model = strings.TrimSpace(model)
			if model == "" { return fmt.Errorf("capacity adapter %q contains an empty model", capability) }
			if !seen[model] { clean[capability] = append(clean[capability], model); seen[model] = true }
			if len(clean[capability]) > 16 { return fmt.Errorf("capacity adapter %q supports at most 16 models", capability) }
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.data.CapacityAdapters = clean
	return d.saveLocked()
}

func (d *adminStore) setSettings(defaultProvider, saver, code string) error {
	d.mu.Lock(); defer d.mu.Unlock()
	found:=false; for _,p:=range d.data.Providers { if p.ID==defaultProvider { found=true; break } }
	if !found { return errors.New("default provider does not exist") }
	if saver!="safe" && saver!="off" { return errors.New("token saver must be safe or off") }
	if code!="off" && code!="lite" && code!="full" && code!="ultra" { return errors.New("code mode must be off, lite, full or ultra") }
	d.data.DefaultProvider=defaultProvider; d.data.DefaultTokenSaver=saver; d.data.DefaultCodeMode=code
	return d.saveLocked()
}
func (d *adminStore) appendRequest(row requestRecord) {
	d.appendRequests([]requestRecord{row})
}

// appendRequests adds one logical batch to the bounded history and wakes the
// coalescing persistence worker only once for the entire batch.
func (d *adminStore) appendRequests(rows []requestRecord) {
	if d == nil || len(rows) == 0 {
		return
	}
	d.mu.Lock()
	d.data.Requests = append(d.data.Requests, rows...)
	if len(d.data.Requests) > 5000 {
		d.data.Requests = append([]requestRecord(nil), d.data.Requests[len(d.data.Requests)-5000:]...)
	}
	d.dirty = true
	d.mu.Unlock()

	if d.flushWake != nil {
		select {
		case d.flushWake <- struct{}{}:
		default:
		}
	}
}

func (d *adminStore) runPersistence() {
	defer close(d.done)
	var timer *time.Timer
	var timerC <-chan time.Time
	for {
		select {
		case <-d.flushWake:
			if timer == nil {
				timer = time.NewTimer(time.Second)
			} else {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(time.Second)
			}
			timerC = timer.C
		case <-timerC:
			d.flushRequests()
			timer = nil
			timerC = nil
		case <-d.stop:
			if timer != nil {
				timer.Stop()
			}
			d.flushRequests()
			return
		}
	}
}

func (d *adminStore) flushRequests() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.dirty {
		return
	}
	if err := d.saveLocked(); err != nil && d.logger != nil {
		d.logger.Error("could not persist request history batch", "error", err)
	}
}

func (d *adminStore) Close() {
	if d.stop == nil {
		return
	}
	d.closeOnce.Do(func() { close(d.stop) })
	<-d.done
}
func (d *adminStore) keyValid(key string) bool {
	hash:=sha256.Sum256([]byte(key)); encoded:=hex.EncodeToString(hash[:])
	d.mu.RLock(); defer d.mu.RUnlock()
	for _,item:=range d.data.ClientKeys { if item.Enabled && subtleStringEqual(item.Hash,encoded) { return true } }
	return false
}
func subtleStringEqual(a,b string) bool {
	if len(a)!=len(b) { return false }
	var diff byte
	for i:=range a { diff |= a[i]^b[i] }
	return diff==0
}
func (d *adminStore) createKey(name string) (map[string]any,error) {
	buf:=make([]byte,32); if _,err:=rand.Read(buf); err!=nil { return nil,err }
	key:="ftm_live_"+hex.EncodeToString(buf)
	hash:=sha256.Sum256([]byte(key))
	row:=storedClientKey{ID:hex.EncodeToString(hash[:8]),Name:name,Prefix:key[:16],Hash:hex.EncodeToString(hash[:]),Enabled:true,CreatedAt:time.Now().UTC()}
	d.mu.Lock(); defer d.mu.Unlock()
	d.data.ClientKeys=append(d.data.ClientKeys,row)
	if err:=d.saveLocked(); err!=nil { return nil,err }
	return map[string]any{"id":row.ID,"name":row.Name,"prefix":row.Prefix,"enabled":row.Enabled,"created_at":row.CreatedAt,"key":key},nil
}
func (d *adminStore) listKeys() []map[string]any {
	d.mu.RLock(); defer d.mu.RUnlock()
	out:=make([]map[string]any,0,len(d.data.ClientKeys))
	for _,k:=range d.data.ClientKeys { out=append(out,map[string]any{"id":k.ID,"name":k.Name,"prefix":k.Prefix,"enabled":k.Enabled,"created_at":k.CreatedAt}) }
	return out
}
func (d *adminStore) revokeKey(id string) error {
	d.mu.Lock(); defer d.mu.Unlock()
	for i:=range d.data.ClientKeys { if d.data.ClientKeys[i].ID==id { d.data.ClientKeys[i].Enabled=false; return d.saveLocked() } }
	return os.ErrNotExist
}
func (d *adminStore) appendProviderFromInput(in providerInput, existing *config.Provider) (config.Provider,error) {
	id:=strings.ToLower(strings.TrimSpace(in.ID))
	if id=="" { id=strings.ToLower(strings.TrimSpace(in.Name)) }
	id=strings.ReplaceAll(id," ","-")
	if id=="" { return config.Provider{},errors.New("provider id is required") }
	base:=strings.TrimRight(strings.TrimSpace(in.BaseURL),"/")
	u,err:=url.ParseRequestURI(base)
	if err!=nil || u.Host=="" || (u.Scheme!="https" && u.Scheme!="http") { return config.Provider{},errors.New("base URL must be a valid HTTP or HTTPS URL") }
	chatPath:=strings.TrimSpace(in.ChatPath); if chatPath=="" { chatPath="/chat/completions" }
	if !strings.HasPrefix(chatPath,"/") { return config.Provider{},errors.New("chat path must start with /") }
	auth:=strings.TrimSpace(in.AuthType); if auth=="" { auth="bearer" }
	if auth!="bearer" && auth!="none" { return config.Provider{},errors.New("auth type must be bearer or none") }
	models:=in.Models; if len(models)==0 { models=[]string{"*"} }
	for i:=range models { models[i]=strings.TrimSpace(models[i]) }
	timeout:=in.TimeoutSeconds; if timeout<=0 { timeout=100 }; if timeout>600 { return config.Provider{},errors.New("timeout cannot exceed 600 seconds") }
	cleanCapabilities := make(map[string]config.ModelCapabilities, len(in.ModelCapabilities))
	for model, caps := range in.ModelCapabilities {
		model = strings.TrimSpace(model)
		if model == "" { return config.Provider{}, errors.New("model capability key cannot be empty") }
		if caps.ContextWindow < 0 || caps.ContextWindow > 10000000 || caps.MaxOutput < 0 || caps.MaxOutput > 1000000 {
			return config.Provider{}, fmt.Errorf("model %q has invalid context/output limits", model)
		}
		cleanCapabilities[model] = caps
	}
	in.ModelCapabilities = cleanCapabilities
	cleanPricing := make(map[string]config.ModelPricing, len(in.ModelPricing))
	for model, pricing := range in.ModelPricing {
		model = strings.TrimSpace(model)
		if model == "" { return config.Provider{}, errors.New("model pricing key cannot be empty") }
		if pricing.InputPerMillionUSD < 0 || pricing.OutputPerMillionUSD < 0 || pricing.InputPerMillionUSD > 1000000 || pricing.OutputPerMillionUSD > 1000000 {
			return config.Provider{}, fmt.Errorf("model %q has invalid pricing; rates must be between 0 and 1000000 USD per million tokens", model)
		}
		cleanPricing[model] = pricing
	}
	in.ModelPricing = cleanPricing
	p:=config.Provider{ID:id,BaseURL:base,ChatPath:chatPath,ModelsURL:strings.TrimSpace(in.ModelsURL),AuthType:auth,Models:models,TimeoutSeconds:timeout,ModelCapabilities:cloneModelCapabilities(in.ModelCapabilities),ModelPricing:cloneModelPricing(in.ModelPricing)}
	if existing!=nil {
		if in.APIKey=="" && len(in.APIKeys)==0 { p.APIKeys=append([]string(nil),existing.APIKeys...) }
		p.Headers=existing.Headers
		if in.ModelCapabilities == nil { p.ModelCapabilities=cloneModelCapabilities(existing.ModelCapabilities) }
		if in.ModelPricing == nil { p.ModelPricing=cloneModelPricing(existing.ModelPricing) }
	}
	if in.APIKey!="" { p.APIKeys=[]string{strings.TrimSpace(in.APIKey)} }
	if len(in.APIKeys)>0 { p.APIKeys=append([]string(nil),in.APIKeys...) }
	if p.AuthType=="bearer" && len(p.APIKeys)==0 && existing==nil { return config.Provider{},errors.New("API key is required for bearer authentication") }
	if p.ModelsURL!="" { mu,e:=url.ParseRequestURI(p.ModelsURL); if e!=nil || mu.Host=="" || (mu.Scheme!="https" && mu.Scheme!="http") { return config.Provider{},errors.New("models URL must be a valid HTTP or HTTPS URL") } }
	return p,nil
}
func sortedProviders(items []persistedProvider) []persistedProvider {
	out:=append([]persistedProvider(nil),items...)
	sort.Slice(out,func(i,j int)bool{return out[i].ID<out[j].ID})
	return out
}

type providerInput struct {
	ID string `json:"id"`
	Name string `json:"name"`
	BaseURL string `json:"base_url"`
	ChatPath string `json:"chat_path"`
	ModelsURL string `json:"models_url"`
	AuthType string `json:"auth_type"`
	APIKey string `json:"api_key"`
	APIKeys []string `json:"api_keys"`
	Models []string `json:"models"`
	TimeoutSeconds int `json:"timeout_seconds"`
	ModelCapabilities map[string]config.ModelCapabilities `json:"model_capabilities"`
	ModelPricing map[string]config.ModelPricing `json:"model_pricing"`
}
