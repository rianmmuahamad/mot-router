package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// CatalogModel is a model shown in the public landing page and quickstart.
type CatalogModel struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Icon        string `json:"icon"`
	Color       string `json:"color"`
	Enabled     bool   `json:"enabled"`
}

// CatalogPackage is a public offer card. It does not grant or change account access.
type CatalogPackage struct {
	Name        string             `json:"name"`
	Label       string             `json:"label"`
	Price       string             `json:"price"`
	Description string             `json:"description"`
	Features    CatalogFeatureList `json:"features"`
	ModelScope  string             `json:"model_scope,omitempty"`
	Models      []string           `json:"models,omitempty"`
	Button      string             `json:"button"`
	URL         string             `json:"url"`
	Enabled     bool               `json:"enabled"`
}

// CatalogFeatureList accepts the current JSON array and legacy admin forms
// that submitted the textarea as one string. This lets an open, pre-fix page
// save its edits after the backend is updated without losing its form state.
type CatalogFeatureList []string

func (f *CatalogFeatureList) UnmarshalJSON(data []byte) error {
	*f = nil
	if len(data) > 0 && data[0] == '"' {
		var text string
		if err := json.Unmarshal(data, &text); err != nil {
			return err
		}
		separator := "\n"
		if !strings.Contains(text, separator) {
			separator = ","
		}
		for _, item := range strings.Split(text, separator) {
			if item = strings.TrimSpace(item); item != "" {
				*f = append(*f, item)
			}
		}
		return nil
	}
	var items []string
	if err := json.Unmarshal(data, &items); err != nil {
		return err
	}
	*f = items
	return nil
}

type Catalog struct {
	Models   []CatalogModel   `json:"models"`
	Packages []CatalogPackage `json:"packages"`
}

var defaultCatalog = Catalog{
	Models: []CatalogModel{
		{ID: "deepseek-v4.1-flash-free", Name: "DeepSeek V4.1 Flash", Description: "Penalaran mendalam untuk analisis dan problem solving.", Icon: "D", Color: "#2483ff", Enabled: true},
		{ID: "mimo-v2.6-flash-free", Name: "MiMo 2.6 Flash", Description: "Jawaban kilat untuk pekerjaan ringan dan volume tinggi.", Icon: "Mi", Color: "#9d91ff", Enabled: true},
		{ID: "space-bunny-free", Name: "Space Bunny", Description: "Penalaran mendalam untuk analisis dan problem solving.", Icon: "S", Color: "#ff7547", Enabled: true},
		{ID: "jev-1.13-free", Name: "JEV 1.13", Description: "Keputusan terstruktur lengkap dengan confidence score.", Icon: "J", Color: "#f4d35e", Enabled: true},
	},
	Packages: []CatalogPackage{{Name: "Akses 24 jam", Label: "Akses 24 jam", Price: "Rp25.000", Description: "Sekali bayar · aktif satu hari penuh", Features: CatalogFeatureList{"Seluruh model yang tersedia", "Endpoint OpenAI & Anthropic", "Streaming token-by-token", "Halaman status penggunaan pribadi", "Tanpa biaya per token"}, Button: "Aktifkan via WhatsApp", URL: "https://wa.me/6281399939783?text=Halo%20Arbyte!%20Saya%20mau%20aktifkan%20akses%2024%20jam%20Rp25.000", Enabled: true}},
}

func (s *Store) ensureCatalog(ctx context.Context) error {
	for key, value := range map[string]any{"models": defaultCatalog.Models, "packages": defaultCatalog.Packages} {
		b, err := json.Marshal(value)
		if err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO catalog_settings (key,data) VALUES (?,?)`, key, string(b)); err != nil {
			return fmt.Errorf("seed catalog %s: %w", key, err)
		}
	}
	return nil
}

func (s *Store) GetCatalog(ctx context.Context) (*Catalog, error) {
	c := &Catalog{}
	var models, packages string
	if err := s.db.QueryRowContext(ctx, `SELECT data FROM catalog_settings WHERE key='models'`).Scan(&models); err != nil {
		return nil, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT data FROM catalog_settings WHERE key='packages'`).Scan(&packages); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(models), &c.Models); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(packages), &c.Packages); err != nil {
		return nil, err
	}
	return c, nil
}

func (s *Store) SaveCatalog(ctx context.Context, c Catalog) error {
	models, err := json.Marshal(c.Models)
	if err != nil {
		return err
	}
	packages, err := json.Marshal(c.Packages)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for key, value := range map[string]string{"models": string(models), "packages": string(packages)} {
		if _, err = tx.ExecContext(ctx, `INSERT INTO catalog_settings (key,data) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET data=excluded.data`, key, value); err != nil {
			return err
		}
	}
	return tx.Commit()
}
