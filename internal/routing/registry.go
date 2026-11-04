package routing

import (
	"fmt"

	"github.com/mojitex/fantom/internal/config"
)

type Target struct {
	ProviderID    string
	UpstreamModel string
}

func Resolve(model string, routes map[string]config.Route, defaultProvider string, providers map[string]config.Provider) (Target, error) {
	if model == "" { return Target{}, fmt.Errorf("model is required") }
	if route, ok := routes[model]; ok {
		provider, exists := providers[route.Provider]
		if !exists { return Target{}, fmt.Errorf("route references unknown provider %q", route.Provider) }
		upstream := route.UpstreamModel
		if upstream == "" { upstream = model }
		if !allows(provider.Models, upstream) { return Target{}, fmt.Errorf("upstream model %q is not enabled for provider %q", upstream, provider.ID) }
		return Target{ProviderID: provider.ID, UpstreamModel: upstream}, nil
	}
	if defaultProvider == "" { return Target{}, fmt.Errorf("no route configured for model %q", model) }
	provider, exists := providers[defaultProvider]
	if !exists { return Target{}, fmt.Errorf("default provider %q is unavailable", defaultProvider) }
	if !allows(provider.Models, model) { return Target{}, fmt.Errorf("model %q is not enabled for provider %q", model, provider.ID) }
	return Target{ProviderID: provider.ID, UpstreamModel: model}, nil
}

func allows(models []string, model string) bool {
	for _, candidate := range models {
		if candidate == "*" || candidate == model { return true }
	}
	return false
}
