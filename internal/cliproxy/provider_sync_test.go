package cliproxy

import (
	"testing"

	fantomconfig "github.com/mojitex/fantom/internal/config"
)

func TestCompatibilityForProviderMapsNestedChatPathAndCredentialPool(t *testing.T) {
	provider := fantomconfig.Provider{
		ID: "OpenCode-Console", BaseURL: "https://opencode.ai/inference",
		ChatPath: "/openai/v1/chat/completions", AuthType: "bearer",
		APIKeys: []string{"first-secret", "second-secret"},
		Models: []string{"kimi-k3", "glm-5", "*"},
		Headers: map[string]string{"X-Client": "fantom"},
	}
	got, include, err := compatibilityForProvider(provider)
	if err != nil { t.Fatalf("compatibilityForProvider() error = %v", err) }
	if !include { t.Fatal("expected OpenCode provider to be mirrored") }
	if got.Name != "fantom-opencode-console" { t.Fatalf("provider name = %q", got.Name) }
	if got.Prefix != managedProviderPrefixForID("OpenCode-Console") { t.Fatalf("provider prefix = %q", got.Prefix) }
	if got.BaseURL != "https://opencode.ai/inference/openai/v1" { t.Fatalf("base URL = %q", got.BaseURL) }
	if len(got.APIKeyEntries) != 2 { t.Fatalf("API key entries = %d, want 2", len(got.APIKeyEntries)) }
	if got.APIKeyEntries[0].APIKey != "first-secret" || got.APIKeyEntries[1].APIKey != "second-secret" {
		t.Fatal("API key pool order was not preserved")
	}
	if len(got.Models) != 2 || got.Models[0].Name != "kimi-k3" || got.Models[0].Alias != "kimi-k3" || got.Models[1].Name != "glm-5" {
		t.Fatalf("unexpected model aliases: %#v", got.Models)
	}
	if got.Headers["X-Client"] != "fantom" { t.Fatal("provider headers were not copied") }
}

func TestCompatibilityForProviderSkipsBearerWithoutCredentials(t *testing.T) {
	provider := fantomconfig.Provider{
		ID: "missing-key", BaseURL: "https://api.example.com/v1",
		ChatPath: "/chat/completions", AuthType: "bearer", Models: []string{"example-model"},
	}
	_, include, err := compatibilityForProvider(provider)
	if err != nil { t.Fatalf("compatibilityForProvider() error = %v", err) }
	if include { t.Fatal("bearer provider without credentials must stay on the legacy plane") }
}

func TestCompatibilityForProviderAllowsNoAuthAndSkipsNonstandardChatPath(t *testing.T) {
	provider := fantomconfig.Provider{
		ID: "local", BaseURL: "http://127.0.0.1:11434/v1",
		ChatPath: "/chat/completions", AuthType: "none", Models: []string{"llama3"},
	}
	got, include, err := compatibilityForProvider(provider)
	if err != nil || !include { t.Fatalf("no-auth provider mapping: include=%v err=%v", include, err) }
	if len(got.APIKeyEntries) != 0 || got.BaseURL != provider.BaseURL { t.Fatalf("unexpected no-auth mapping: %#v", got) }
	provider.ID = "custom-path"
	provider.ChatPath = "/generate"
	_, include, err = compatibilityForProvider(provider)
	if err != nil { t.Fatalf("nonstandard path mapping error = %v", err) }
	if include { t.Fatal("provider with a non-OpenAI chat path must remain on the legacy plane") }
}


func TestCompatibilityForProviderSkipsWildcardWithoutResolvedModels(t *testing.T) {
	provider := fantomconfig.Provider{
		ID: "wildcard-only",
		BaseURL: "https://api.example.com/v1",
		ChatPath: "/chat/completions",
		AuthType: "none",
		Models: []string{"*"},
	}
	_, include, err := compatibilityForProvider(provider)
	if err != nil {
		t.Fatalf("compatibilityForProvider() error = %v", err)
	}
	if include {
		t.Fatal("provider with an unresolved wildcard catalog must not publish an empty model group")
	}
}


func TestEngineModelForProviderUsesNamespacedMapping(t *testing.T) {
	engine := &Engine{managedModels: map[string]map[string]string{
		"opencode-console": {
			"kimi-k3": "fantom-opencode-abc123/kimi-k3",
		},
	}}
	if got := engine.ModelForProvider("OpenCode-Console", "kimi-k3"); got != "fantom-opencode-abc123/kimi-k3" {
		t.Fatalf("ModelForProvider() = %q", got)
	}
	if got := engine.ModelForProvider("missing", "kimi-k3"); got != "" {
		t.Fatalf("unknown provider mapping = %q, want empty", got)
	}
}
