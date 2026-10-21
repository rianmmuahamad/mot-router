package server

import (
	"context"
	"io"
	"net/http"
	"strings"
)

// providerFor decides which upstream serves a request.
//
// Resolution order:
//  1. Explicit provider prefixes (including opencode-zen/).
//  2. Provider-specific model namespaces (zai/stealth/stepfun/poolside).
//  3. Token Harbor models (tokenharbor/th prefixes and the known :free ids).
//  4. Zen catalog match, else the configured default.
func (s *Server) providerFor(ctx context.Context, model string) (provider, bareModel string) {
	low := strings.ToLower(model)
	for _, prefix := range []string{"codebuddy/", "codebuddy-intl/", "cbai/"} {
		if strings.HasPrefix(low, prefix) {
			return ProviderCodeBuddy, model[len(prefix):]
		}
	}
	// An explicit provider must win over model-family names and catalog lookups.
	if strings.HasPrefix(low, ProviderZen+"/") {
		return ProviderZen, model[len(ProviderZen)+1:]
	}
	if strings.HasPrefix(low, "cline/") {
		bare := model[len("cline/"):]
		// This advertised Cline shorthand refers to its canonical provider ID.
		if bare == "glm-5.2" {
			bare = "zai/glm-5.2"
		}
		return ProviderCline, bare
	}
	if strings.HasPrefix(model, "cline-free/") || strings.HasPrefix(model, "zai/") || strings.HasPrefix(model, "stealth/") || strings.HasPrefix(model, "stepfun/") || strings.HasPrefix(model, "poolside/") {
		return ProviderCline, model
	}
	if strings.HasPrefix(model, "tokenharbor/") {
		return ProviderTokenHarbor, strings.TrimPrefix(model, "tokenharbor/")
	}
	if strings.HasPrefix(model, "th/") {
		return ProviderTokenHarbor, strings.TrimPrefix(model, "th/")
	}
	// A `:free` suffix only means Token Harbor when Token Harbor can actually
	// serve it.
	//
	// It used to route unconditionally, so `mimo-v2.6-flash:free` answered
	// "Token Harbor is not configured" even though Zen serves that model
	// itself — the model is in the Zen catalog, and a forced
	// `opencode-zen/mimo-v2.6-flash-free` request returns 200. Falling through
	// to the catalog lookup below keeps the model usable with no provider
	// configuration at all.
	if strings.HasSuffix(model, ":free") {
		if len(s.tokenHarborKeys()) > 0 {
			return ProviderTokenHarbor, strings.TrimPrefix(strings.TrimPrefix(model, "tokenharbor/"), "th/")
		}
		if s.zenModelExists(ctx, model) {
			return ProviderZen, model
		}
		// Also try the Zen naming convention: Zen serves `mimo-v2.6-flash-free`
		// where Token Harbor advertises `mimo-v2.6-flash:free`.
		if alt := strings.TrimSuffix(model, ":free") + "-free"; s.zenModelExists(ctx, alt) {
			return ProviderZen, alt
		}
		return ProviderTokenHarbor, strings.TrimPrefix(strings.TrimPrefix(model, "tokenharbor/"), "th/")
	}
	if p := s.providerForCatalog(ctx, model); p != "" {
		return p, model
	}
	return s.effectiveProvider(), model
}

// providerForCatalog reports which provider owns a bare model id, or "" when the
// id is unknown to every catalog.
func (s *Server) providerForCatalog(ctx context.Context, model string) string {
	if model == "" {
		return ""
	}
	if s.zenModelExists(ctx, model) {
		return ProviderZen
	}
	return ""
}

// flushWriter forwards Flush to the underlying ResponseWriter so streamed
// frames reach the client immediately.
type flushWriter struct {
	w       io.Writer
	flusher http.Flusher
}

func (f *flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if f.flusher != nil {
		f.flusher.Flush()
	}
	return n, err
}

// zenModelExists reports whether the Zen catalog contains a bare model id.
// The result is cached by the zen client, so this is cheap after the first call.
func (s *Server) zenModelExists(ctx context.Context, model string) bool {
	models, err := s.zen.ListModels(ctx)
	if err != nil {
		return false
	}
	for _, m := range models {
		if m.ID == model {
			return true
		}
	}
	return false
}
