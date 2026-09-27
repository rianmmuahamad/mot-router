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
//  1. Cline models (glm/zai/stealth/stepfun/poolside prefixes).
//  2. Token Harbor models (tokenharbor/th prefixes and the known :free ids).
//  3. Zen catalog match, else the configured default.
func (s *Server) providerFor(ctx context.Context, model string) (provider, bareModel string) {
	low := strings.ToLower(model)
	if strings.Contains(low, "glm") {
		return ProviderCline, "zai/glm-5.2"
	}
	if strings.HasPrefix(model, "cline/") {
		return ProviderCline, strings.TrimPrefix(model, "cline/")
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
	if strings.Contains(model, "deepseek-v4.1-flash:free") || strings.Contains(model, "qwen3.8-flash:free") || strings.Contains(model, "mimo-v2.6-flash:free") || strings.HasSuffix(model, ":free") {
		return ProviderTokenHarbor, strings.TrimPrefix(strings.TrimPrefix(model, "tokenharbor/"), "th/")
	}
	if strings.HasPrefix(model, ProviderZen+"/") {
		return ProviderZen, strings.TrimPrefix(model, ProviderZen+"/")
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
