package cliproxy

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	fantomconfig "github.com/mojitex/fantom/internal/config"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

const managedProviderPrefix = "fantom-"

func managedProviderPrefixForID(id string) string {
	normalized := strings.ToLower(strings.TrimSpace(id))
	var slug strings.Builder
	lastDash := false
	for _, r := range normalized {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			slug.WriteRune(r)
			lastDash = false
		default:
			if slug.Len() > 0 && !lastDash {
				slug.WriteByte('-')
				lastDash = true
			}
		}
	}
	value := strings.Trim(slug.String(), "-")
	if value == "" {
		value = "provider"
	}
	sum := sha256.Sum256([]byte(normalized))
	return managedProviderPrefix + value + "-" + hex.EncodeToString(sum[:3])
}

func compatibilityForProvider(provider fantomconfig.Provider) (sdkconfig.OpenAICompatibility, bool, error) {
	provider.ID = strings.ToLower(strings.TrimSpace(provider.ID))
	if provider.ID == "" {
		return sdkconfig.OpenAICompatibility{}, false, errors.New("provider ID is required")
	}
	if provider.AuthType == "bearer" && len(provider.APIKeys) == 0 {
		return sdkconfig.OpenAICompatibility{}, false, nil
	}
	baseURL := strings.TrimRight(strings.TrimSpace(provider.BaseURL), "/")
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return sdkconfig.OpenAICompatibility{}, false, fmt.Errorf("provider %q has an invalid base URL", provider.ID)
	}
	chatPath := strings.TrimRight(strings.TrimSpace(provider.ChatPath), "/")
	if chatPath == "" {
		chatPath = "/chat/completions"
	}
	const suffix = "/chat/completions"
	if !strings.HasSuffix(chatPath, suffix) {
		// The SDK executor uses the OpenAI-compatible /chat/completions path.
		// Nonstandard paths remain served by Fantom's legacy gateway.
		return sdkconfig.OpenAICompatibility{}, false, nil
	}
	pathPrefix := strings.TrimSuffix(chatPath, suffix)
	if pathPrefix != "" {
		baseURL += "/" + strings.Trim(pathPrefix, "/")
	}
	models := make([]sdkconfig.OpenAICompatibilityModel, 0, len(provider.Models))
	seenModels := make(map[string]struct{}, len(provider.Models))
	for _, model := range provider.Models {
		model = strings.TrimSpace(model)
		if model == "" || model == "*" {
			continue
		}
		if _, exists := seenModels[model]; exists {
			continue
		}
		seenModels[model] = struct{}{}
		models = append(models, sdkconfig.OpenAICompatibilityModel{Name: model, Alias: model})
	}
	headers := make(map[string]string, len(provider.Headers))
	for name, value := range provider.Headers {
		name = strings.TrimSpace(name)
		if name != "" && !strings.EqualFold(name, "Authorization") {
			headers[name] = value
		}
	}
	if len(models) == 0 {
		// A wildcard provider whose catalog is temporarily empty stays on the
		// legacy Fantom route instead of publishing an empty SDK model group.
		return sdkconfig.OpenAICompatibility{}, false, nil
	}
	if len(headers) == 0 {
		headers = nil
	}
	compat := sdkconfig.OpenAICompatibility{
		Name: managedProviderPrefix + provider.ID, Prefix: managedProviderPrefixForID(provider.ID), BaseURL: baseURL,
		Models: models, Headers: headers,
	}
	if provider.AuthType == "bearer" {
		compat.APIKeyEntries = make([]sdkconfig.OpenAICompatibilityAPIKey, 0, len(provider.APIKeys))
		for _, key := range provider.APIKeys {
			key = strings.TrimSpace(key)
			if key != "" {
				compat.APIKeyEntries = append(compat.APIKeyEntries, sdkconfig.OpenAICompatibilityAPIKey{APIKey: key})
			}
		}
		if len(compat.APIKeyEntries) == 0 {
			return sdkconfig.OpenAICompatibility{}, false, nil
		}
	}
	return compat, true, nil
}

// SyncProviders atomically updates the Fantom-owned OpenAI-compatible provider
// entries in the CLIProxyAPI config. Other entries made in its advanced console
// are preserved. The SDK file watcher applies credential and model changes live.
func (e *Engine) SyncProviders(providers []fantomconfig.Provider) error {
	if e == nil || strings.TrimSpace(e.configPath) == "" {
		return errors.New("embedded router configuration is unavailable")
	}
	e.syncMu.Lock()
	defer e.syncMu.Unlock()
	cfg, err := sdkconfig.LoadConfig(e.configPath)
	if err != nil {
		return fmt.Errorf("load embedded router config for provider sync: %w", err)
	}
	preserved := make([]sdkconfig.OpenAICompatibility, 0, len(cfg.OpenAICompatibility))
	for _, entry := range cfg.OpenAICompatibility {
		if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(entry.Name)), managedProviderPrefix) {
			preserved = append(preserved, entry)
		}
	}
	managed := make([]sdkconfig.OpenAICompatibility, 0, len(providers))
	managedModels := make(map[string]map[string]string, len(providers))
	for _, provider := range providers {
		entry, include, err := compatibilityForProvider(provider)
		if err != nil {
			return err
		}
		if !include {
			continue
		}
		managed = append(managed, entry)
		models := make(map[string]string, len(entry.Models))
		for _, model := range entry.Models {
			if model.Name != "" {
				models[model.Name] = entry.Prefix + "/" + model.Name
			}
		}
		managedModels[strings.ToLower(strings.TrimSpace(provider.ID))] = models
	}
	sort.Slice(managed, func(i, j int) bool { return managed[i].Name < managed[j].Name })
	cfg.OpenAICompatibility = append(preserved, managed...)
	if err := sdkconfig.SaveConfigPreserveComments(e.configPath, cfg); err != nil {
		return fmt.Errorf("save embedded provider registry: %w", err)
	}
	if err := os.Chmod(e.configPath, 0o600); err != nil {
		return fmt.Errorf("secure embedded provider registry: %w", err)
	}
	e.mu.Lock()
	e.managedModels = managedModels
	e.cachedAt = time.Time{}
	e.mu.Unlock()
	return nil
}
