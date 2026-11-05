package server

import (
	"context"
	"net/http"
	"strings"

	cliproxyengine "github.com/mojitex/fantom/internal/cliproxy"
)

// CoreRouter is the narrow integration boundary between Fantom's dashboard/auth
// layer and the embedded protocol/OAuth runtime.
type CoreRouter interface {
	APIProxy() http.Handler
	ManagementProxy() http.Handler
	Models(context.Context) ([]cliproxyengine.Model, error)
	HasModel(context.Context, string) bool
	Close()
}

// AttachCore installs the embedded CLIProxyAPI service before the public listener starts.
func (s *Server) AttachCore(core CoreRouter) {
	s.core = core
}

func (s *Server) handleCoreAPI(w http.ResponseWriter, r *http.Request) {
	if s.core == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "embedded routing engine is not ready", "service_unavailable")
		return
	}
	s.core.APIProxy().ServeHTTP(w, r)
}

func (s *Server) handleCoreManagement(w http.ResponseWriter, r *http.Request) {
	if s.core == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "embedded management engine is not ready", "service_unavailable")
		return
	}
	s.core.ManagementProxy().ServeHTTP(w, r)
}

func (s *Server) handleCoreCallback(w http.ResponseWriter, r *http.Request) {
	if s.core == nil {
		http.Error(w, "OAuth callback service unavailable", http.StatusServiceUnavailable)
		return
	}
	s.core.APIProxy().ServeHTTP(w, r)
}

// legacyModelMatch returns true only for an explicit Fantom route or a model
// already known by one of Fantom's legacy providers. It never performs a network
// catalog refresh while deciding which runtime should own an inference request.
func (s *Server) legacyModelMatch(_ context.Context, model string) bool {
	state := s.store.snapshot()
	if _, ok := state.Routes[model]; ok {
		return true
	}
	for _, saved := range state.Providers {
		provider := s.providerByID(saved.ID)
		if provider == nil {
			continue
		}
		for _, configured := range provider.config.Models {
			if strings.TrimSpace(configured) == model && configured != "*" {
				return true
			}
		}
		provider.catalogMu.Lock()
		for _, discovered := range provider.catalogModels {
			if discovered == model {
				provider.catalogMu.Unlock()
				return true
			}
		}
		provider.catalogMu.Unlock()
	}
	return false
}
