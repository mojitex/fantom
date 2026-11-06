package cliproxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"
)

// Model is the normalized model-list entry returned by CLIProxyAPI.
type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`
}

// Engine embeds the upstream Go routing runtime behind Fantom's public listener.
// The internal listener is loopback-only; callers must pass through Fantom auth.
type Engine struct {
	service    *cliproxy.Service
	cancel     context.CancelFunc
	apiKey         string
	managementKey string
	configPath     string
	baseURL    *url.URL
	syncMu     sync.Mutex
	apiProxy   *httputil.ReverseProxy
	adminProxy *httputil.ReverseProxy
	client     *http.Client

	mu          sync.RWMutex
	cached      []Model
	cachedAt    time.Time
	refreshMu   sync.Mutex
	runErrors   chan error
}

type modelList struct {
	Data []Model `json:"data"`
}

// New builds an isolated CLIProxyAPI runtime with persistent OAuth/config storage.
// It never overwrites an existing CLIProxyAPI configuration except for Fantom-owned
// listener and authentication settings.
func New(adminKey string) (*Engine, error) {
	adminKey = strings.TrimSpace(adminKey)
	if adminKey == "" {
		return nil, errors.New("Fantom admin key is required to initialize the router engine")
	}

	dataDir := strings.TrimSpace(os.Getenv("FANTOM_DATA_DIR"))
	if dataDir == "" {
		dataDir = "./data"
	}
	coreDir := filepath.Join(dataDir, "cli-proxy")
	authDir := filepath.Join(coreDir, "auth")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		return nil, fmt.Errorf("create CLIProxyAPI data directory: %w", err)
	}
	if err := os.Chmod(coreDir, 0o700); err != nil {
		return nil, fmt.Errorf("secure CLIProxyAPI data directory: %w", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("reserve internal router port: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		return nil, fmt.Errorf("release reserved internal router port: %w", err)
	}

	internalKey := strings.TrimSpace(os.Getenv("FANTOM_ROUTER_INTERNAL_KEY"))
	if internalKey == "" {
		sum := sha256.Sum256([]byte("fantom-cli-proxy-core-v1:" + adminKey))
		internalKey = "ftm_internal_" + hex.EncodeToString(sum[:])
	}
	managementSecret, err := bcrypt.GenerateFromPassword([]byte(adminKey), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("hash embedded management key: %w", err)
	}
	managementSecretHash := string(managementSecret)

	publicURL := strings.TrimRight(strings.TrimSpace(os.Getenv("FANTOM_PUBLIC_URL")), "/")
	configPath := filepath.Join(coreDir, "config.yaml")
	if _, err := os.Stat(configPath); errors.Is(err, os.ErrNotExist) {
		initial := map[string]any{
			"config-version": 8,
			"server": map[string]any{
				"host": "127.0.0.1",
				"port": port,
			},
			"oauth": map[string]any{
				"auth-dir": authDir,
			},
			"access": map[string]any{
				"api-keys": []string{internalKey},
			},
			"observability": map[string]any{
				"logs": map[string]any{"request-log": true},
				"usage": map[string]any{"usage-statistics-enabled": true},
			},
			"routing": map[string]any{
				"strategy": "weighted-round-robin",
				"session-affinity": true,
				"session-affinity-ttl": "1h",
			},
			"management": map[string]any{
				"allow-remote": true,
				"secret-key": managementSecretHash,
				"disable-control-panel": false,
				"disable-auto-update-panel": true,
				"base-url": publicURL,
			},
		}
		content, marshalErr := yaml.Marshal(initial)
		if marshalErr != nil {
			return nil, fmt.Errorf("encode CLIProxyAPI bootstrap config: %w", marshalErr)
		}
		if err := writePrivateFile(configPath, content); err != nil {
			return nil, fmt.Errorf("write CLIProxyAPI bootstrap config: %w", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("inspect CLIProxyAPI config: %w", err)
	}

	cfg, err := sdkconfig.LoadConfig(configPath)
	if err != nil {
		return nil, fmt.Errorf("load CLIProxyAPI config: %w", err)
	}
	// Fantom owns the internal listener and the only credential accepted on it.
	// User-facing client keys are validated by Fantom before requests are proxied.
	cfg.Host = "127.0.0.1"
	cfg.Port = port
	cfg.AuthDir = authDir
	cfg.APIKeys = []string{internalKey}
	cfg.RemoteManagement.SecretKey = managementSecretHash
	cfg.RemoteManagement.AllowRemote = true
	cfg.RemoteManagement.DisableAutoUpdatePanel = true
	if publicURL != "" {
		cfg.RemoteManagement.BaseURL = publicURL
	}
	cfg.RequestLog = true
	cfg.UsageStatisticsEnabled = true
	if strings.TrimSpace(cfg.Routing.Strategy) == "" {
		cfg.Routing.Strategy = "weighted-round-robin"
	}
	if err := sdkconfig.SaveConfigPreserveComments(configPath, cfg); err != nil {
		return nil, fmt.Errorf("persist CLIProxyAPI runtime settings: %w", err)
	}
	if err := os.Chmod(configPath, 0o600); err != nil {
		return nil, fmt.Errorf("secure CLIProxyAPI configuration file: %w", err)
	}

	service, err := cliproxy.NewBuilder().
		WithConfig(cfg).
		WithConfigPath(configPath).
		Build()
	if err != nil {
		return nil, fmt.Errorf("build CLIProxyAPI runtime: %w", err)
	}

	target, err := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", port))
	if err != nil {
		return nil, fmt.Errorf("create internal router URL: %w", err)
	}
	apiProxy := httputil.NewSingleHostReverseProxy(target)
	apiDirector := apiProxy.Director
	apiProxy.Director = func(req *http.Request) {
		apiDirector(req)
		req.Header.Set("Authorization", "Bearer "+internalKey)
		req.Header.Set("X-Fantom-Core", "cliproxyapi")
	}
	apiProxy.FlushInterval = -1
	apiProxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, proxyErr error) {
		http.Error(w, "Fantom routing engine unavailable", http.StatusBadGateway)
	}

	adminProxy := httputil.NewSingleHostReverseProxy(target)
	adminProxy.FlushInterval = -1
	adminProxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, proxyErr error) {
		http.Error(w, "Fantom management engine unavailable", http.StatusBadGateway)
	}

	return &Engine{
		service: service,
		apiKey: internalKey,
		managementKey: adminKey,
		configPath: configPath,
		baseURL: target,
		apiProxy: apiProxy,
		adminProxy: adminProxy,
		client: &http.Client{Timeout: 8 * time.Second},
		runErrors: make(chan error, 1),
	}, nil
}

// Start launches the embedded runtime and waits until its private API is ready.
func (e *Engine) Start(parent context.Context) error {
	if e == nil || e.service == nil {
		return errors.New("router engine is not initialized")
	}
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	e.cancel = cancel
	go func() {
		if err := e.service.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			select {
			case e.runErrors <- err:
			default:
			}
		}
	}()

	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		request, requestErr := http.NewRequestWithContext(ctx, http.MethodGet, e.baseURL.String()+"/healthz", nil)
		if requestErr == nil {
			response, doErr := e.client.Do(request)
			if doErr == nil {
				_ = response.Body.Close()
				if response.StatusCode >= 200 && response.StatusCode < 300 {
					// The service starts its config watcher just after the HTTP listener.
					// Let it attach before Fantom writes mirrored provider configuration.
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-time.After(250 * time.Millisecond):
					}
					return nil
				}
			}
		}
		select {
		case runErr := <-e.runErrors:
			cancel()
			return fmt.Errorf("CLIProxyAPI runtime stopped during startup: %w", runErr)
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			cancel()
			return errors.New("CLIProxyAPI runtime did not become ready within 30 seconds")
		case <-ticker.C:
		}
	}
}

// APIProxy is used only after Fantom has authenticated an application key.
func (e *Engine) APIProxy() http.Handler {
	if e == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "routing engine unavailable", http.StatusServiceUnavailable)
		})
	}
	return e.apiProxy
}

// ManagementProxy preserves the Fantom administrator bearer token. The embedded
// runtime independently validates it against remote-management.secret-key.
func (e *Engine) ManagementProxy() http.Handler {
	if e == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "management engine unavailable", http.StatusServiceUnavailable)
		})
	}
	return e.adminProxy
}

// Models returns the embedded runtime's catalog with a short cache to keep model
// listing and model dispatch from creating request-time catalog storms.
func (e *Engine) Models(ctx context.Context) ([]Model, error) {
	if e == nil {
		return nil, errors.New("routing engine is unavailable")
	}
	e.mu.RLock()
	if time.Since(e.cachedAt) < 15*time.Second {
		result := append([]Model(nil), e.cached...)
		e.mu.RUnlock()
		return result, nil
	}
	e.mu.RUnlock()

	e.refreshMu.Lock()
	defer e.refreshMu.Unlock()
	e.mu.RLock()
	if time.Since(e.cachedAt) < 15*time.Second {
		result := append([]Model(nil), e.cached...)
		e.mu.RUnlock()
		return result, nil
	}
	e.mu.RUnlock()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, e.baseURL.String()+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+e.apiKey)
	response, err := e.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("read embedded model catalog: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("embedded model catalog returned HTTP %d", response.StatusCode)
	}
	var payload modelList
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode embedded model catalog: %w", err)
	}
	e.mu.Lock()
	e.cached = append([]Model(nil), payload.Data...)
	e.cachedAt = time.Now()
	result := append([]Model(nil), e.cached...)
	e.mu.Unlock()
	return result, nil
}

// UsageQueue drains normalized usage events from the embedded management API.
// The SDK queue is destructive, so callers should persist returned records promptly.
func (e *Engine) UsageQueue(ctx context.Context, count int) ([]json.RawMessage, error) {
	if e == nil || e.baseURL == nil {
		return nil, errors.New("embedded usage engine is unavailable")
	}
	if count <= 0 {
		count = 100
	}
	if count > 500 {
		count = 500
	}
	endpoint := e.baseURL.String() + "/v8/management/observability/usage/queue?count=" + strconv.Itoa(count)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("create embedded usage request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+e.managementKey)
	response, err := e.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("read embedded usage queue: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return nil, fmt.Errorf("embedded usage queue returned HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	var records []json.RawMessage
	if err := json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(&records); err != nil {
		return nil, fmt.Errorf("decode embedded usage queue: %w", err)
	}
	return records, nil
}

func (e *Engine) HasModel(ctx context.Context, id string) bool {
	models, err := e.Models(ctx)
	if err != nil {
		return false
	}
	for _, item := range models {
		if item.ID == id {
			return true
		}
	}
	return false
}

// Close cancels background refreshers and gracefully stops the embedded service.
func (e *Engine) Close() {
	if e == nil {
		return
	}
	if e.cancel != nil {
		e.cancel()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if e.service != nil {
		if err := e.service.Shutdown(ctx); err != nil {
			// Shutdown is best-effort here; the outer gateway owns process termination.
		}
	}
}

func writePrivateFile(path string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, content, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}
