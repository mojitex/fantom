package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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
	Headers        map[string]string `json:"headers,omitempty"`
	APIKeys        []string          `json:"api_keys,omitempty"`
}

func persistProvider(p config.Provider) persistedProvider {
	return persistedProvider{ID:p.ID, BaseURL:p.BaseURL, ChatPath:p.ChatPath, ModelsURL:p.ModelsURL, AuthType:p.AuthType, Models:append([]string(nil),p.Models...), TimeoutSeconds:p.TimeoutSeconds, Headers:p.Headers, APIKeys:append([]string(nil),p.APIKeys...)}
}
func (p persistedProvider) runtime() config.Provider {
	return config.Provider{ID:p.ID, BaseURL:p.BaseURL, ChatPath:p.ChatPath, ModelsURL:p.ModelsURL, AuthType:p.AuthType, Models:append([]string(nil),p.Models...), TimeoutSeconds:p.TimeoutSeconds, Headers:p.Headers, APIKeys:append([]string(nil),p.APIKeys...)}
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
	ID        string    `json:"id"`
	At        time.Time `json:"at"`
	Provider  string    `json:"provider"`
	Model     string    `json:"model"`
	Status    int       `json:"status"`
	LatencyMS int64     `json:"latency_ms"`
	Streaming bool      `json:"streaming"`
}

type adminState struct {
	Version            int                `json:"version"`
	DefaultProvider    string             `json:"default_provider"`
	Providers          []persistedProvider `json:"providers"`
	Routes             map[string]config.Route `json:"routes"`
	DefaultTokenSaver  string             `json:"default_token_saver"`
	DefaultCodeMode    string             `json:"default_code_mode"`
	ClientKeys         []storedClientKey  `json:"client_keys"`
	Requests           []requestRecord    `json:"requests"`
}

type adminStore struct {
	mu   sync.RWMutex
	path string
	data adminState
}

func newAdminStore(cfg *config.Config) (*adminStore, error) {
	dir := strings.TrimSpace(os.Getenv("FANTOM_DATA_DIR"))
	if dir == "" { dir = "./data" }
	path := filepath.Join(dir, "fantom-state.json")
	store := &adminStore{path:path}
	store.data = adminState{Version:1, DefaultProvider:cfg.DefaultProvider, Routes:cfg.Routes, DefaultTokenSaver:cfg.DefaultTokenSaver, DefaultCodeMode:cfg.DefaultCodeMode}
	for _, p := range cfg.Providers { store.data.Providers = append(store.data.Providers, persistProvider(p)) }
	if store.data.Routes == nil { store.data.Routes = map[string]config.Route{} }
	raw, err := os.ReadFile(path)
	if err == nil {
		var loaded adminState
		if decodeErr := json.Unmarshal(raw, &loaded); decodeErr != nil { return nil, fmt.Errorf("decode Fantom state: %w", decodeErr) }
		if loaded.Version != 1 { return nil, fmt.Errorf("unsupported Fantom state version %d", loaded.Version) }
		if loaded.Routes == nil { loaded.Routes = map[string]config.Route{} }
		if loaded.DefaultTokenSaver == "" { loaded.DefaultTokenSaver = cfg.DefaultTokenSaver }
		if loaded.DefaultCodeMode == "" { loaded.DefaultCodeMode = cfg.DefaultCodeMode }
		if len(loaded.Providers) == 0 { loaded.Providers = store.data.Providers }
		if loaded.DefaultProvider == "" { loaded.DefaultProvider = cfg.DefaultProvider }
		store.data = loaded
	} else if !errors.Is(err, os.ErrNotExist) { return nil, fmt.Errorf("read Fantom state: %w", err) }
	if err := store.saveLocked(); err != nil { return nil, err }
	return store, nil
}

func (d *adminStore) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(d.path), 0700); err != nil { return fmt.Errorf("create data directory: %w",err) }
	raw, err := json.MarshalIndent(d.data, "", "  ")
	if err != nil { return err }
	tmp := d.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0600); err != nil { return fmt.Errorf("write state: %w",err) }
	if err := os.Rename(tmp, d.path); err != nil { _ = os.Remove(tmp); return fmt.Errorf("commit state: %w",err) }
	return nil
}

func (d *adminStore) snapshot() adminState {
	d.mu.RLock(); defer d.mu.RUnlock()
	out := d.data
	out.Providers = append([]persistedProvider(nil), d.data.Providers...)
	out.Routes = make(map[string]config.Route,len(d.data.Routes)); for k,v := range d.data.Routes { out.Routes[k]=v }
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
	d.mu.Lock(); defer d.mu.Unlock()
	providers:=map[string]bool{}; for _,p:=range d.data.Providers { providers[p.ID]=true }
	for alias,route:=range routes {
		if strings.TrimSpace(alias)=="" { return errors.New("model alias cannot be empty") }
		if !providers[route.Provider] { return fmt.Errorf("route %q references unknown provider %q",alias,route.Provider) }
		if strings.TrimSpace(route.UpstreamModel)=="" { return fmt.Errorf("route %q needs an upstream model",alias) }
	}
	d.data.Routes=routes
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
	d.mu.Lock(); defer d.mu.Unlock()
	d.data.Requests=append(d.data.Requests,row)
	if len(d.data.Requests)>5000 { d.data.Requests=append([]requestRecord(nil),d.data.Requests[len(d.data.Requests)-5000:]...) }
	_ = d.saveLocked()
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
	p:=config.Provider{ID:id,BaseURL:base,ChatPath:chatPath,ModelsURL:strings.TrimSpace(in.ModelsURL),AuthType:auth,Models:models,TimeoutSeconds:timeout}
	if existing!=nil {
		if in.APIKey=="" && len(in.APIKeys)==0 { p.APIKeys=append([]string(nil),existing.APIKeys...) }
		p.Headers=existing.Headers
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
}
