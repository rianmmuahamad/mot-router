package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/rian/motgate/internal/store"
)

var catalogID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/-]{0,119}$`)
var catalogColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

func (s *Server) handleCatalogPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(s.catalogHTML)
}

func (s *Server) handlePublicCatalog(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	c, err := s.store.GetCatalog(ctx)
	if err != nil {
		writeAPIError(w, 500, "unable to load public catalog", "api_error")
		return
	}
	models := make([]store.CatalogModel, 0, len(c.Models))
	for _, m := range c.Models {
		if m.Enabled {
			models = append(models, m)
		}
	}
	packages := make([]store.CatalogPackage, 0, len(c.Packages))
	for _, p := range c.Packages {
		if p.Enabled {
			packages = append(packages, p)
		}
	}
	w.Header().Set("Cache-Control", "public, max-age=30, stale-while-revalidate=120")
	writeJSON(w, 200, store.Catalog{Models: models, Packages: packages})
}

func (s *Server) handleAdminCatalog(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	c, err := s.store.GetCatalog(ctx)
	if err != nil {
		writeAPIError(w, 500, "unable to load catalog", "api_error")
		return
	}
	writeJSON(w, 200, c)
}

func (s *Server) handleSaveAdminCatalog(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 256*1024)
	var c store.Catalog
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		writeAPIError(w, 400, "invalid catalog JSON: "+err.Error(), "invalid_request_error")
		return
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeAPIError(w, 400, "request must contain one JSON object", "invalid_request_error")
		return
	}
	if err := validateCatalog(c); err != nil {
		writeAPIError(w, 400, err.Error(), "invalid_request_error")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := s.store.SaveCatalog(ctx, c); err != nil {
		writeAPIError(w, 500, "unable to save catalog", "api_error")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]any{"ok": true})
}

func validateCatalog(c store.Catalog) error {
	if len(c.Models) > 200 {
		return errText("maksimal 200 model")
	}
	if len(c.Packages) > 10 {
		return errText("maksimal 10 paket")
	}
	seen := map[string]bool{}
	modelIDs := map[string]bool{}
	for i, m := range c.Models {
		if strings.TrimSpace(m.ID) == "" || !catalogID.MatchString(m.ID) {
			return errText("ID model ke-%d tidak valid", i+1)
		}
		if seen[strings.ToLower(m.ID)] {
			return errText("ID model tidak boleh duplikat: %s", m.ID)
		}
		seen[strings.ToLower(m.ID)] = true
		modelIDs[m.ID] = true
		if !validText(m.Name, 1, 80) || !validText(m.Description, 1, 240) {
			return errText("nama/deskripsi model ke-%d wajib diisi dan terlalu panjang", i+1)
		}
		if !validText(m.Icon, 1, 4) || !catalogColor.MatchString(m.Color) {
			return errText("ikon atau warna model ke-%d tidak valid", i+1)
		}
	}
	for i, p := range c.Packages {
		if p.ModelScope != "" && p.ModelScope != "all" && p.ModelScope != "selected" {
			return errText("pilihan model paket ke-%d tidak valid", i+1)
		}
		if len(p.Models) > 200 {
			return errText("maksimal 200 model per paket")
		}
		if p.ModelScope == "selected" {
			if len(p.Models) == 0 {
				return errText("pilih minimal satu model untuk paket ke-%d", i+1)
			}
			selected := map[string]bool{}
			for _, id := range p.Models {
				if !modelIDs[id] || selected[id] {
					return errText("model %q pada paket ke-%d tidak ada di katalog atau duplikat", id, i+1)
				}
				selected[id] = true
			}
		}
		if !validText(p.Name, 1, 80) || !validText(p.Label, 1, 80) || !validText(p.Price, 1, 60) || !validText(p.Description, 1, 180) || !validText(p.Button, 1, 60) {
			return errText("lengkapi teks paket ke-%d (batas 180 karakter)", i+1)
		}
		if len(p.Features) > 12 {
			return errText("maksimal 12 fitur per paket")
		}
		for _, f := range p.Features {
			if !validText(f, 1, 160) {
				return errText("fitur paket ke-%d kosong atau melebihi 160 karakter", i+1)
			}
		}
		u, err := url.Parse(p.URL)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return errText("tautan paket ke-%d harus URL HTTPS yang valid", i+1)
		}
	}
	return nil
}

func errText(format string, args ...any) error {
	return errors.New(strings.TrimSpace(strings.ReplaceAll(fmt.Sprintf(format, args...), "\n", " ")))
}
func validText(v string, min, max int) bool {
	n := len([]rune(strings.TrimSpace(v)))
	return n >= min && n <= max
}

// Only administrators query the live catalogue; public landing requests stay local.
func (s *Server) handleRouterCatalog(w http.ResponseWriter, r *http.Request) {
	base := s.cfg.ModelCatalogURL
	if base == "" {
		base = "http://127.0.0.1:8787"
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/v1/models", nil)
	if err != nil {
		writeAPIError(w, 502, "alamat katalog router tidak valid", "api_error")
		return
	}
	client := &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		writeAPIError(w, 502, "katalog router belum dapat dihubungi; model tersimpan tetap tersedia", "api_error")
		return
	}
	defer resp.Body.Close()
	var payload struct {
		Data []struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			Provider string `json:"owned_by"`
		} `json:"data"`
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024+1))
	if err != nil || len(raw) > 2*1024*1024 || resp.StatusCode != 200 || json.Unmarshal(raw, &payload) != nil || payload.Data == nil {
		writeAPIError(w, 502, "respons katalog router tidak valid", "api_error")
		return
	}
	type model struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Provider string `json:"provider"`
	}
	models := make([]model, 0)
	seen := map[string]bool{}
	for _, m := range payload.Data {
		key := strings.ToLower(m.ID)
		if !catalogID.MatchString(m.ID) || seen[key] {
			continue
		}
		seen[key] = true
		name := m.Name
		if !validText(name, 1, 80) {
			name = m.ID
			if len([]rune(name)) > 80 {
				name = string([]rune(name)[:80])
			}
		}
		provider := m.Provider
		if !validText(provider, 1, 80) {
			provider = "router"
		}
		models = append(models, model{m.ID, name, provider})
		if len(models) >= 200 {
			break
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]any{"models": models})
}
