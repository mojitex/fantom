package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadSupportsDistinctProviderModelEndpoint(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "providers.json")
	data := []byte(`{
	  "listen_addr": ":8080",
	  "default_provider": "nvidia",
	  "providers": [
	    {"id":"nvidia","base_url":"https://integrate.api.nvidia.com/v1","models":["*"],"timeout_seconds":100},
	    {"id":"opencode-console","base_url":"https://opencode.ai/inference/openai/v1","chat_path":"/chat/completions","models_url":"https://opencode.ai/inference/v1/models","api_key_env":"OPENCODE_API_KEY","models":["kimi-k3"]}
	  ],
	  "routes": {}
	}`)
	if err := os.WriteFile(path, data, 0600); err != nil { t.Fatal(err) }
	t.Setenv("CONFIG_FILE", path)
	t.Setenv("GATEWAY_API_KEY", "test-gateway-key")
	t.Setenv("NVIDIA_API_KEY", "test-nvidia-key")
	t.Setenv("OPENCODE_API_KEY", "oc_sk_test")
	t.Setenv("FANTOM_TOKEN_SAVER", "safe")
	t.Setenv("FANTOM_CODE_MODE", "off")
	t.Setenv("FANTOM_MAX_CONCURRENT_REQUESTS", "")
	t.Setenv("FANTOM_MAX_CONCURRENT_UPSTREAM_REQUESTS", "")
	t.Setenv("FANTOM_MAX_CONCURRENT_PER_PROVIDER", "")

	cfg, err := Load()
	if err != nil { t.Fatalf("Load() error = %v", err) }
	if len(cfg.Providers) != 2 { t.Fatalf("provider count = %d, want exactly 2", len(cfg.Providers)) }
	second := cfg.Providers[1]
	if second.ModelsURL != "https://opencode.ai/inference/v1/models" { t.Fatalf("models_url = %q", second.ModelsURL) }
	if second.ChatPath != "/chat/completions" || len(second.APIKeys) != 1 || second.APIKeys[0] != "oc_sk_test" {
		t.Fatalf("OpenCode Console provider was not loaded: %#v", second)
	}
	if cfg.DefaultTokenSaver != "safe" { t.Fatalf("default token saver = %q, want safe", cfg.DefaultTokenSaver) }
	if cfg.MaxConcurrentRequests != 256 { t.Fatalf("max concurrent requests = %d, want 256", cfg.MaxConcurrentRequests) }
	if cfg.MaxConcurrentUpstreamRequests != 512 { t.Fatalf("max concurrent upstream requests = %d, want 512", cfg.MaxConcurrentUpstreamRequests) }
	if cfg.MaxConcurrentPerProvider != 64 { t.Fatalf("max concurrent per provider = %d, want 64", cfg.MaxConcurrentPerProvider) }
}
