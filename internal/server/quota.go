package server

import (
	"database/sql"
	"net/http"
	"strings"
	"time"
)

type quotaSummary struct {
	TotalRequests     int64   `json:"total_requests"`
	TotalInputTokens  int64   `json:"total_input_tokens"`
	TotalOutputTokens int64   `json:"total_output_tokens"`
	TotalTokens       int64   `json:"total_tokens"`
	SuccessCount      int64   `json:"success_count"`
	LimitCount        int64   `json:"limit_count"`
	ErrorCount        int64   `json:"error_count"`
	SuccessRate       float64 `json:"success_rate"`
	AvgLatencyMs      int64   `json:"avg_latency_ms"`
	TodayRequests     int64   `json:"today_requests"`
	TodayInputTokens  int64   `json:"today_input_tokens"`
	TodayOutputTokens int64   `json:"today_output_tokens"`
	TodayTokens       int64   `json:"today_tokens"`
	TodayLimits       int64   `json:"today_limits"`
}

type providerQuotaView struct {
	Name         string         `json:"name"`
	Requests     int64          `json:"requests"`
	InputTokens  int64          `json:"input_tokens"`
	OutputTokens int64          `json:"output_tokens"`
	TotalTokens  int64          `json:"total_tokens"`
	SuccessCount int64          `json:"success_count"`
	LimitCount   int64          `json:"limit_count"`
	Status       string         `json:"status"`
	Extra        map[string]any `json:"extra,omitempty"`
}

type modelQuotaView struct {
	Model        string  `json:"model"`
	Requests     int64   `json:"requests"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	TotalTokens  int64   `json:"total_tokens"`
	SuccessCount int64   `json:"success_count"`
	LimitCount   int64   `json:"limit_count"`
	TokenShare   float64 `json:"token_share"`
}

type dailyQuotaView struct {
	Date         string `json:"date"`
	Requests     int64  `json:"requests"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	TotalTokens  int64  `json:"total_tokens"`
	LimitCount   int64  `json:"limit_count"`
}

type clientKeyQuotaView struct {
	KeyID        int64  `json:"key_id"`
	KeyName      string `json:"key_name"`
	Requests     int64  `json:"requests"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	TotalTokens  int64  `json:"total_tokens"`
	LastUsedAt   int64  `json:"last_used_at"`
}

type modelFreeAllowanceView struct {
	Model          string `json:"model"`
	Name           string `json:"name"`
	Provider       string `json:"provider"`
	Type           string `json:"type"`
	Banner         string `json:"banner"`
	Description    string `json:"description"`
	Status         string `json:"status"`
	UsedPct        int    `json:"used_pct"`
	TokensConsumed int64  `json:"tokens_consumed"`
	RequestsCount  int64  `json:"requests_count"`
	ResetInfo      string `json:"reset_info"`
}

type quotaReport struct {
	Summary        quotaSummary                 `json:"summary"`
	Providers      map[string]providerQuotaView `json:"providers"`
	FreeAllowances []modelFreeAllowanceView     `json:"free_allowances"`
	Models         []modelQuotaView             `json:"models"`
	Daily          []dailyQuotaView             `json:"daily"`
	ClientKeys     []clientKeyQuotaView         `json:"client_keys"`
}

func (s *Server) qwenPoolStatus() (total, active, penalized int, keys []map[string]any) {
	qwenKeys := s.tokenHarborQwenKeys()
	thQwenMu.Lock()
	defer thQwenMu.Unlock()
	now := time.Now()
	for _, k := range qwenKeys {
		info := map[string]any{"key": redactKey(k)}
		if exp, bad := thQwenPenalties[k]; bad && now.Before(exp) {
			penalized++
			info["status"] = "penalized_429"
			info["cooldown_remaining_sec"] = int64(time.Until(exp).Seconds())
		} else {
			active++
			info["status"] = "ready"
			info["cooldown_remaining_sec"] = int64(0)
		}
		keys = append(keys, info)
	}
	return len(qwenKeys), active, penalized, keys
}

func (s *Server) handleQuotaStats(w http.ResponseWriter, r *http.Request) {
	db := s.store.DB()
	if db == nil {
		writeAPIError(w, http.StatusInternalServerError, "database not available", "internal_error")
		return
	}

	var rep quotaReport
	rep.Providers = make(map[string]providerQuotaView)

	var avgLat sql.NullFloat64
	row := db.QueryRow(`
		SELECT
			COUNT(*),
			COALESCE(SUM(input_tokens), 0),
			COALESCE(SUM(output_tokens), 0),
			COALESCE(SUM(CASE WHEN status = 200 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status = 429 THEN 1 ELSE 0 END), 0),
			COALESCE(AVG(latency_ms), 0)
		FROM requests
	`)
	_ = row.Scan(
		&rep.Summary.TotalRequests,
		&rep.Summary.TotalInputTokens,
		&rep.Summary.TotalOutputTokens,
		&rep.Summary.SuccessCount,
		&rep.Summary.LimitCount,
		&avgLat,
	)
	rep.Summary.TotalTokens = rep.Summary.TotalInputTokens + rep.Summary.TotalOutputTokens
	rep.Summary.ErrorCount = rep.Summary.TotalRequests - rep.Summary.SuccessCount - rep.Summary.LimitCount
	if rep.Summary.TotalRequests > 0 {
		rep.Summary.SuccessRate = float64(rep.Summary.SuccessCount) * 100.0 / float64(rep.Summary.TotalRequests)
	}
	rep.Summary.AvgLatencyMs = int64(avgLat.Float64)

	now := time.Now().UTC()
	startOfToday := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).UnixMilli()

	todayRow := db.QueryRow(`
		SELECT
			COUNT(*),
			COALESCE(SUM(input_tokens), 0),
			COALESCE(SUM(output_tokens), 0),
			COALESCE(SUM(CASE WHEN status = 429 THEN 1 ELSE 0 END), 0)
		FROM requests
		WHERE ts >= ?
	`, startOfToday)
	_ = todayRow.Scan(
		&rep.Summary.TodayRequests,
		&rep.Summary.TodayInputTokens,
		&rep.Summary.TodayOutputTokens,
		&rep.Summary.TodayLimits,
	)
	rep.Summary.TodayTokens = rep.Summary.TodayInputTokens + rep.Summary.TodayOutputTokens

	provRows, err := db.Query(`
		SELECT
			provider,
			COUNT(*),
			COALESCE(SUM(input_tokens), 0),
			COALESCE(SUM(output_tokens), 0),
			COALESCE(SUM(CASE WHEN status = 200 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status = 429 THEN 1 ELSE 0 END), 0)
		FROM requests
		GROUP BY provider
	`)
	if err == nil {
		defer provRows.Close()
		for provRows.Next() {
			var p providerQuotaView
			_ = provRows.Scan(&p.Name, &p.Requests, &p.InputTokens, &p.OutputTokens, &p.SuccessCount, &p.LimitCount)
			p.TotalTokens = p.InputTokens + p.OutputTokens
			p.Status = "active"
			if p.LimitCount > 0 && p.Requests > 0 && float64(p.LimitCount)/float64(p.Requests) > 0.5 {
				p.Status = "rate_limited"
			}
			rep.Providers[p.Name] = p
		}
	}

	thKeys := s.tokenHarborKeys()
	thActive := 0
	thPenalized := 0
	type thKeyDetail struct {
		Key                  string `json:"key"`
		Status               string `json:"status"`
		CooldownRemainingSec int64  `json:"cooldown_remaining_sec"`
	}
	var thKeyDetails []thKeyDetail

	thMu.Lock()
	nowTime := time.Now()
	for _, k := range thKeys {
		var kd thKeyDetail
		kd.Key = redactKey(k)
		if exp, penalized := thPenalties[k]; penalized && nowTime.Before(exp) {
			thPenalized++
			kd.Status = "penalized_429"
			kd.CooldownRemainingSec = int64(time.Until(exp).Seconds())
		} else {
			thActive++
			kd.Status = "ready"
		}
		thKeyDetails = append(thKeyDetails, kd)
	}
	thMu.Unlock()

	qTotal, qActive, qPenalized, qKeys := s.qwenPoolStatus()
	thProv := rep.Providers[ProviderTokenHarbor]
	thProv.Name = ProviderTokenHarbor
	thProv.Extra = map[string]any{
		"total_keys":     len(thKeys),
		"active_keys":    thActive,
		"penalized_keys": thPenalized,
		"keys":           thKeyDetails,
		"base_url":       s.tokenHarborBaseURL(),
		"qwen_pool": map[string]any{
			"total_keys":     qTotal,
			"active_keys":    qActive,
			"penalized_keys": qPenalized,
			"dedicated":      s.tokenHarborQwenDedicated(),
			"keys":           qKeys,
		},
	}
	rep.Providers[ProviderTokenHarbor] = thProv

	clineKey := s.clineAPIKey()
	clineProv := rep.Providers[ProviderCline]
	clineProv.Name = ProviderCline
	clineProv.Extra = map[string]any{
		"configured": clineKey != "",
		"api_key":    redactKey(clineKey),
		"base_url":   s.clineBaseURL(),
	}
	rep.Providers[ProviderCline] = clineProv

	zenProv := rep.Providers[ProviderZen]
	zenProv.Name = ProviderZen
	zenExtra := map[string]any{
		"model": s.DefaultModel(),
		"mode":  "free_tier",
	}
	if s.mihomoEnabled() {
		zenExtra["egress_mode"] = "proxy_rotation"
	} else {
		zenExtra["egress_mode"] = "direct"
	}
	zenProv.Extra = zenExtra
	rep.Providers[ProviderZen] = zenProv

	modelStats := make(map[string]struct {
		reqs   int64
		tokens int64
		limits int64
	})
	mRows, err := db.Query(`
		SELECT model, COUNT(*), COALESCE(SUM(input_tokens + output_tokens), 0), COALESCE(SUM(CASE WHEN status = 429 THEN 1 ELSE 0 END), 0)
		FROM requests GROUP BY model
	`)
	if err == nil {
		defer mRows.Close()
		for mRows.Next() {
			var m string
			var r, tok, lim int64
			_ = mRows.Scan(&m, &r, &tok, &lim)
			modelStats[m] = struct {
				reqs   int64
				tokens int64
				limits int64
			}{r, tok, lim}
		}
	}

	rollingResetDate := "Rolling 7-day period"
	var errText sql.NullString
	_ = db.QueryRow(`
		SELECT error FROM requests
		WHERE provider = 'tokenharbor' AND error LIKE '%next rolling 7-day period starts on%'
		ORDER BY id DESC LIMIT 1
	`).Scan(&errText)
	if errText.Valid && errText.String != "" {
		if idx := strings.Index(errText.String, "starts on "); idx >= 0 {
			part := errText.String[idx+10:]
			if endIdx := strings.Index(part, "."); endIdx >= 0 {
				rollingResetDate = strings.TrimSpace(part[:endIdx])
			}
		}
	}

	qwenStat := modelStats["qwen3.8-flash:free"]
	dsStat := modelStats["deepseek-v4.1-flash:free"]
	mimoStat := modelStats["mimo-v2.6-flash:free"]
	museStat := modelStats["muse-spark-1.3-contributor-free"]
	bunnyStat := modelStats["space-bunny-free"]
	geminiClineStat := modelStats["cline-free/gemini-3.8-flash"]
	deepseekClineStat := modelStats["cline-free/deepseek-v4.1-flash"]
	glmClineStat := modelStats["zai/glm-5.2"]

	qwenReset := "Campaign Allowance (Dedicated Key Pool)"
	if !s.tokenHarborQwenDedicated() {
		qwenReset = "Campaign Allowance (Terpisah dari Rolling)"
	}

	rep.FreeAllowances = []modelFreeAllowanceView{
		{
			Model:          "cline/glm-5.2",
			Name:           "GLM 5.2 (Z.ai via Cline Free)",
			Provider:       "Cline",
			Type:           "cline_free",
			Banner:         "Z.ai Frontier Open Weights",
			Description:    "Model frontier reasoning GLM 5.2 dari Z.ai via akun Cline Anda. Konteks 1 Juta token.",
			Status:         "Ready · Cline Tier",
			UsedPct:        0,
			TokensConsumed: glmClineStat.tokens,
			RequestsCount:  glmClineStat.reqs,
			ResetInfo:      "Cline Account Allowance",
		},
		{
			Model:          "cline-free/gemini-3.8-flash",
			Name:           "Gemini 3.8 Flash (Cline Free)",
			Provider:       "Cline",
			Type:           "cline_free",
			Banner:         "Google's Most Intelligent Flash",
			Description:    "Model Flash paling pintar dari Google via Cline API. Berkecepatan tinggi dengan kuota akun.",
			Status:         "Ready · Cline Tier",
			UsedPct:        0,
			TokensConsumed: geminiClineStat.tokens,
			RequestsCount:  geminiClineStat.reqs,
			ResetInfo:      "Cline Account Allowance",
		},
		{
			Model:          "cline-free/deepseek-v4.1-flash",
			Name:           "DeepSeek V4.1 Flash (Cline Free)",
			Provider:       "Cline",
			Type:           "cline_free",
			Banner:         "Cline Free Tier (1M Context)",
			Description:    "Model DeepSeek V4.1 Flash via Cline API dengan konteks 1M token.",
			Status:         "Ready · Cline Tier",
			UsedPct:        0,
			TokensConsumed: deepseekClineStat.tokens,
			RequestsCount:  deepseekClineStat.reqs,
			ResetInfo:      "Cline Account Allowance",
		},
		{
			Model:          "qwen3.8-flash:free",
			Name:           "Qwen3.8 Flash — Limited Time (Dedicated Key)",
			Provider:       "Token Harbor",
			Type:           "campaign",
			Banner:         "Dedicated Qwen Key Pool",
			Description:    "Qwen campaign dilayani pool kunci khusus yang terisolasi dari pool utama, sehingga pause per-akun tidak membakar 27 key utama.",
			Status:         "Dedicated · Qwen-Only Key",
			UsedPct:        0,
			TokensConsumed: qwenStat.tokens,
			RequestsCount:  qwenStat.reqs,
			ResetInfo:      qwenReset,
		},
		{
			Model:          "deepseek-v4.1-flash:free",
			Name:           "DeepSeek V4.1 Flash (Free Tier)",
			Provider:       "Token Harbor",
			Type:           "rolling_allowance",
			Banner:         "Rolling 7-Day Allowance",
			Description:    "Menggunakan jatah rolling free allowance 7 hari Token Harbor. Otomatis failover antar kunci saat 429.",
			Status:         "Ready · Rolling Tier",
			UsedPct:        0,
			TokensConsumed: dsStat.tokens,
			RequestsCount:  dsStat.reqs,
			ResetInfo:      "Next reset: " + rollingResetDate,
		},
		{
			Model:          "mimo-v2.6-flash:free",
			Name:           "MiMo V2.6 Flash (Free Tier)",
			Provider:       "Token Harbor / Zen",
			Type:           "rolling_allowance",
			Banner:         "Rolling 7-Day Allowance",
			Description:    "Model gratis berkecepatan tinggi dengan konteks hingga 1M token.",
			Status:         "Ready · Rolling Tier",
			UsedPct:        0,
			TokensConsumed: mimoStat.tokens,
			RequestsCount:  mimoStat.reqs,
			ResetInfo:      "Next reset: " + rollingResetDate,
		},
		{
			Model:          "muse-spark-1.3-contributor-free",
			Name:           "Muse Spark 1.3 (Zen Community)",
			Provider:       "OpenCode Zen",
			Type:           "community_zen",
			Banner:         "Unlimited Free (Rotated Egress)",
			Description:    "OpenCode Zen contributor tier, didukung rotasi otomatis 2.000 IP pool mihomo.",
			Status:         "Active · Auto-Heal",
			UsedPct:        0,
			TokensConsumed: museStat.tokens,
			RequestsCount:  museStat.reqs,
			ResetInfo:      "Auto-Heal / No Weekly Ceiling",
		},
		{
			Model:          "space-bunny-free",
			Name:           "Space Bunny (Zen Community)",
			Provider:       "OpenCode Zen",
			Type:           "community_zen",
			Banner:         "Unlimited Free (Chat Completions)",
			Description:    "Format OpenAI chat completions dengan adaptasi gate otomatis.",
			Status:         "Active · Auto-Heal",
			UsedPct:        0,
			TokensConsumed: bunnyStat.tokens,
			RequestsCount:  bunnyStat.reqs,
			ResetInfo:      "Auto-Heal / No Weekly Ceiling",
		},
	}

	modRows, err := db.Query(`
		SELECT
			model,
			COUNT(*),
			COALESCE(SUM(input_tokens), 0),
			COALESCE(SUM(output_tokens), 0),
			COALESCE(SUM(CASE WHEN status = 200 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status = 429 THEN 1 ELSE 0 END), 0)
		FROM requests
		GROUP BY model
		ORDER BY (SUM(input_tokens) + SUM(output_tokens)) DESC
		LIMIT 15
	`)
	if err == nil {
		defer modRows.Close()
		for modRows.Next() {
			var m modelQuotaView
			_ = modRows.Scan(&m.Model, &m.Requests, &m.InputTokens, &m.OutputTokens, &m.SuccessCount, &m.LimitCount)
			m.TotalTokens = m.InputTokens + m.OutputTokens
			if rep.Summary.TotalTokens > 0 {
				m.TokenShare = float64(m.TotalTokens) * 100.0 / float64(rep.Summary.TotalTokens)
			}
			rep.Models = append(rep.Models, m)
		}
	}

	sevenDaysAgo := time.Now().UTC().AddDate(0, 0, -6)
	sevenDaysStart := time.Date(sevenDaysAgo.Year(), sevenDaysAgo.Month(), sevenDaysAgo.Day(), 0, 0, 0, 0, time.UTC).UnixMilli()

	dailyRows, err := db.Query(`
		SELECT
			strftime('%Y-%m-%d', ts / 1000, 'unixepoch') AS day,
			COUNT(*),
			COALESCE(SUM(input_tokens), 0),
			COALESCE(SUM(output_tokens), 0),
			COALESCE(SUM(CASE WHEN status = 429 THEN 1 ELSE 0 END), 0)
		FROM requests
		WHERE ts >= ?
		GROUP BY day
		ORDER BY day ASC
	`, sevenDaysStart)
	if err == nil {
		defer dailyRows.Close()
		for dailyRows.Next() {
			var d dailyQuotaView
			_ = dailyRows.Scan(&d.Date, &d.Requests, &d.InputTokens, &d.OutputTokens, &d.LimitCount)
			d.TotalTokens = d.InputTokens + d.OutputTokens
			rep.Daily = append(rep.Daily, d)
		}
	}

	keyRows, err := db.Query(`
		SELECT
			COALESCE(k.id, 0),
			COALESCE(k.name, 'Default/Direct'),
			COUNT(r.id),
			COALESCE(SUM(r.input_tokens), 0),
			COALESCE(SUM(r.output_tokens), 0),
			COALESCE(MAX(r.ts), 0)
		FROM requests r
		LEFT JOIN api_keys k ON r.key_id = k.id
		GROUP BY r.key_id
		ORDER BY (SUM(r.input_tokens) + SUM(r.output_tokens)) DESC
		LIMIT 10
	`)
	if err == nil {
		defer keyRows.Close()
		for keyRows.Next() {
			var ck clientKeyQuotaView
			_ = keyRows.Scan(&ck.KeyID, &ck.KeyName, &ck.Requests, &ck.InputTokens, &ck.OutputTokens, &ck.LastUsedAt)
			ck.TotalTokens = ck.InputTokens + ck.OutputTokens
			rep.ClientKeys = append(rep.ClientKeys, ck)
		}
	}

	writeJSON(w, http.StatusOK, rep)
}
