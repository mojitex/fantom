package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
)

type ModelCapabilities struct {
	Vision        bool `json:"vision,omitempty"`
	PDF           bool `json:"pdf,omitempty"`
	AudioInput    bool `json:"audio_input,omitempty"`
	VideoInput    bool `json:"video_input,omitempty"`
	ImageOutput   bool `json:"image_output,omitempty"`
	AudioOutput   bool `json:"audio_output,omitempty"`
	Search        bool `json:"search,omitempty"`
	Tools         bool `json:"tools,omitempty"`
	Reasoning     bool `json:"reasoning,omitempty"`
	ContextWindow int  `json:"context_window,omitempty"`
	MaxOutput     int  `json:"max_output,omitempty"`
}

type ModelPricing struct {
	InputPerMillionUSD  float64 `json:"input_per_million_usd"`
	OutputPerMillionUSD float64 `json:"output_per_million_usd"`
}

type Provider struct {
	ID                string                       `json:"id"`
	BaseURL           string                       `json:"base_url"`
	ChatPath          string                       `json:"chat_path"`
	ModelsURL         string                       `json:"models_url"`
	AuthType          string                       `json:"auth_type"`
	APIKeyEnv         string                       `json:"api_key_env"`
	APIKeysEnv        string                       `json:"api_keys_env"`
	Models            []string                     `json:"models"`
	TimeoutSeconds    int                          `json:"timeout_seconds"`
	Headers           map[string]string            `json:"headers"`
	ModelCapabilities map[string]ModelCapabilities `json:"model_capabilities,omitempty"`
	ModelPricing      map[string]ModelPricing      `json:"model_pricing,omitempty"`
	APIKeys           []string                     `json:"-"`
}

type Route struct {
	Provider              string   `json:"provider"`
	UpstreamModel         string   `json:"upstream_model"`
	FallbackModels        []string `json:"fallback_models,omitempty"`
	Strategy              string   `json:"strategy,omitempty"`
	StickyLimit           int      `json:"sticky_limit,omitempty"`
	SmartDecisionProvider string   `json:"smart_decision_provider,omitempty"`
	SmartDecisionModel    string   `json:"smart_decision_model,omitempty"`
	SmartPolicy           string   `json:"smart_policy,omitempty"`
	SmartConfidence       float64  `json:"smart_confidence,omitempty"`
	SmartTimeoutSeconds   int      `json:"smart_timeout_seconds,omitempty"`
	FusionModels          []string `json:"fusion_models,omitempty"`
	JudgeModel            string   `json:"judge_model,omitempty"`
	FusionMinPanel        int      `json:"fusion_min_panel,omitempty"`
	FusionTimeoutSeconds  int      `json:"fusion_timeout_seconds,omitempty"`
}

type Config struct {
	ListenAddr         string           `json:"listen_addr"`
	DefaultProvider    string           `json:"default_provider"`
	Providers          []Provider       `json:"providers"`
	Routes             map[string]Route `json:"routes"`
	GatewayAPIKeys     []string         `json:"-"`
	StaticDirectory    string           `json:"-"`
	MaxRequestBytes    int64            `json:"-"`
	MaxConcurrentRequests int            `json:"-"`
	MaxConcurrentPerProvider int         `json:"-"`
	DefaultTokenSaver             string `json:"-"`
	DefaultCodeMode               string `json:"-"`
	DefaultCavemanMode            string `json:"-"`
	HeadroomEnabled               bool   `json:"-"`
	HeadroomURL                   string `json:"-"`
	HeadroomTimeoutSeconds        int    `json:"-"`
	HeadroomCompressUserMessages  bool   `json:"-"`
}

func Load() (*Config, error) {
	path := strings.TrimSpace(os.Getenv("CONFIG_FILE"))
	if path == "" {
		path = "./configs/providers.json"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read provider config %q: %w", path, err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse provider config: %w", err)
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = envOr("GATEWAY_LISTEN_ADDR", ":8080")
	}
	if value := strings.TrimSpace(os.Getenv("GATEWAY_API_KEYS")); value != "" {
		cfg.GatewayAPIKeys = splitCSV(value)
	} else if value := strings.TrimSpace(os.Getenv("GATEWAY_API_KEY")); value != "" {
		cfg.GatewayAPIKeys = []string{value}
	}
	if len(cfg.GatewayAPIKeys) == 0 {
		return nil, errors.New("set GATEWAY_API_KEY or GATEWAY_API_KEYS; the gateway refuses to start without client authentication")
	}
	cfg.StaticDirectory = envOr("STATIC_DIR", "./dashboard/out")
	cfg.MaxRequestBytes = int64(ParsePositiveIntEnv("MAX_REQUEST_MB", 32)) << 20
	cfg.MaxConcurrentRequests = ParsePositiveIntEnv("FANTOM_MAX_CONCURRENT_REQUESTS", 256)
	if cfg.MaxConcurrentRequests > 4096 {
		return nil, errors.New("FANTOM_MAX_CONCURRENT_REQUESTS cannot exceed 4096")
	}
	cfg.MaxConcurrentPerProvider = ParsePositiveIntEnv("FANTOM_MAX_CONCURRENT_PER_PROVIDER", 64)
	if cfg.MaxConcurrentPerProvider > 2048 {
		return nil, errors.New("FANTOM_MAX_CONCURRENT_PER_PROVIDER cannot exceed 2048")
	}
	cfg.DefaultTokenSaver = strings.ToLower(envOr("FANTOM_TOKEN_SAVER", "safe"))
	if cfg.DefaultTokenSaver != "safe" {
		cfg.DefaultTokenSaver = "off"
	}
	cfg.DefaultCodeMode = strings.ToLower(envOr("FANTOM_CODE_MODE", "off"))
	if cfg.DefaultCodeMode != "lite" && cfg.DefaultCodeMode != "full" && cfg.DefaultCodeMode != "ultra" {
		cfg.DefaultCodeMode = "off"
	}
	cfg.DefaultCavemanMode = strings.ToLower(envOr("FANTOM_CAVEMAN_MODE", "off"))
	if cfg.DefaultCavemanMode != "lite" && cfg.DefaultCavemanMode != "full" && cfg.DefaultCavemanMode != "ultra" {
		cfg.DefaultCavemanMode = "off"
	}
	cfg.HeadroomEnabled = envBool("FANTOM_HEADROOM_ENABLED", false)
	cfg.HeadroomURL = strings.TrimSpace(os.Getenv("FANTOM_HEADROOM_URL"))
	cfg.HeadroomTimeoutSeconds = ParsePositiveIntEnv("FANTOM_HEADROOM_TIMEOUT_SECONDS", 3)
	if cfg.HeadroomTimeoutSeconds > 30 {
		return nil, errors.New("FANTOM_HEADROOM_TIMEOUT_SECONDS cannot exceed 30")
	}
	cfg.HeadroomCompressUserMessages = envBool("FANTOM_HEADROOM_COMPRESS_USER_MESSAGES", false)
	if cfg.HeadroomEnabled {
		headroomURL, err := url.ParseRequestURI(cfg.HeadroomURL)
		if err != nil || headroomURL.Host == "" || (headroomURL.Scheme != "https" && headroomURL.Scheme != "http") || headroomURL.User != nil {
			return nil, errors.New("FANTOM_HEADROOM_URL must be an http or https URL without embedded credentials when Headroom is enabled")
		}
	}
	if len(cfg.Providers) == 0 {
		return nil, errors.New("at least one provider must be configured")
	}

	seen := make(map[string]struct{}, len(cfg.Providers))
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		p.ID = strings.TrimSpace(p.ID)
		if p.ID == "" {
			return nil, fmt.Errorf("provider at index %d has an empty id", i)
		}
		if _, exists := seen[p.ID]; exists {
			return nil, fmt.Errorf("duplicate provider id %q", p.ID)
		}
		seen[p.ID] = struct{}{}
		u, err := url.ParseRequestURI(strings.TrimRight(p.BaseURL, "/"))
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
			return nil, fmt.Errorf("provider %q has invalid base_url", p.ID)
		}
		p.BaseURL = strings.TrimRight(p.BaseURL, "/")
		if p.ChatPath == "" {
			p.ChatPath = "/chat/completions"
		}
		if !strings.HasPrefix(p.ChatPath, "/") {
			return nil, fmt.Errorf("provider %q chat_path must start with /", p.ID)
		}
		if p.ModelsURL != "" {
			modelsURL, err := url.ParseRequestURI(p.ModelsURL)
			if err != nil || modelsURL.Host == "" || (modelsURL.Scheme != "https" && modelsURL.Scheme != "http") {
				return nil, fmt.Errorf("provider %q has invalid models_url", p.ID)
			}
		}
		if p.AuthType == "" {
			p.AuthType = "bearer"
		}
		if p.AuthType != "bearer" && p.AuthType != "none" {
			return nil, fmt.Errorf("provider %q auth_type must be bearer or none", p.ID)
		}
		if p.TimeoutSeconds <= 0 {
			p.TimeoutSeconds = 100
		}
		if p.TimeoutSeconds > 600 {
			return nil, fmt.Errorf("provider %q timeout_seconds cannot exceed 600", p.ID)
		}
		if p.AuthType == "bearer" {
			if p.APIKeysEnv != "" {
				p.APIKeys = splitCSV(os.Getenv(p.APIKeysEnv))
			}
			if len(p.APIKeys) == 0 && p.APIKeyEnv != "" {
				if key := strings.TrimSpace(os.Getenv(p.APIKeyEnv)); key != "" {
					p.APIKeys = []string{key}
				}
			}
		}
		if len(p.Models) == 0 {
			return nil, fmt.Errorf("provider %q must declare at least one model or wildcard", p.ID)
		}
	}
	if cfg.DefaultProvider == "" && len(cfg.Providers) == 1 {
		cfg.DefaultProvider = cfg.Providers[0].ID
	}
	if cfg.DefaultProvider != "" {
		if _, ok := seen[cfg.DefaultProvider]; !ok {
			return nil, fmt.Errorf("default_provider %q does not exist", cfg.DefaultProvider)
		}
	}
	for model, route := range cfg.Routes {
		if strings.TrimSpace(model) == "" {
			return nil, errors.New("route model IDs cannot be empty")
		}
		if _, ok := seen[route.Provider]; !ok {
			return nil, fmt.Errorf("route %q references unknown provider %q", model, route.Provider)
		}
	}
	return &cfg, nil
}

func splitCSV(value string) []string {
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if item := strings.TrimSpace(part); item != "" {
			result = append(result, item)
		}
	}
	return result
}

func envBool(key string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" { return fallback }
	parsed, err := strconv.ParseBool(value)
	if err != nil { return fallback }
	return parsed
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func ParsePositiveIntEnv(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	n, err := strconv.Atoi(value)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}
