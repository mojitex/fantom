package routing

import (
	"testing"

	"github.com/mojitex/fantom/internal/config"
)

func TestResolveDefaultProviderWildcard(t *testing.T) {
	providers := map[string]config.Provider{"nvidia": {ID: "nvidia", Models: []string{"*"}}}
	target, err := Resolve("meta/model-x", nil, "nvidia", providers)
	if err != nil { t.Fatalf("Resolve() error = %v", err) }
	if target.ProviderID != "nvidia" || target.UpstreamModel != "meta/model-x" { t.Fatalf("unexpected target: %#v", target) }
}

func TestResolveExplicitAlias(t *testing.T) {
	providers := map[string]config.Provider{"nvidia": {ID: "nvidia", Models: []string{"meta/upstream-model"}}}
	routes := map[string]config.Route{"fast-model": {Provider: "nvidia", UpstreamModel: "meta/upstream-model"}}
	target, err := Resolve("fast-model", routes, "", providers)
	if err != nil { t.Fatalf("Resolve() error = %v", err) }
	if target.UpstreamModel != "meta/upstream-model" { t.Fatalf("unexpected upstream model: %q", target.UpstreamModel) }
}

func TestResolveRejectsDisabledModel(t *testing.T) {
	providers := map[string]config.Provider{"nvidia": {ID: "nvidia", Models: []string{"meta/allowed"}}}
	if _, err := Resolve("meta/blocked", nil, "nvidia", providers); err == nil { t.Fatal("expected model allowlist rejection") }
}
