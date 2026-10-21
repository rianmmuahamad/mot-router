package server

import (
	"context"
	"net/http"
	"time"

	"github.com/rian/mot-router/internal/gate"
	"github.com/rian/mot-router/internal/mihomo"
	"github.com/rian/mot-router/internal/zen"
)

// Keep the node diagnostic independent from a deprecated or non-Zen default.
const proxyProbeModel = gate.DefaultProbeModel

// A diagnostic tests the selected path once. Retrying via a different node
// would attribute the reply to an IP that did not produce it.
func (s *Server) handleMihomoTest(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	started := time.Now()
	out := map[string]any{"ok": false, "proxy_ok": false, "zen_ok": false, "rotated": false, "mode": "single_node"}
	reply := func(stage string, err error) {
		out["elapsed_ms"] = time.Since(started).Milliseconds()
		if stage != "" {
			out["failure_stage"] = stage
		}
		if err != nil {
			out["error"] = err.Error()
		}
		writeJSON(w, http.StatusOK, out)
	}
	c := s.mihomoClient()
	group, err := s.mihomoGroup(ctx, c)
	if err != nil {
		reply("controller", err)
		return
	}
	out["group"] = group
	// Native tunnels carry the selected node explicitly. Legacy controller
	// selection is serialized with other router operations for this small test.
	if !nodeBankRuntimeEnabled() {
		mihomoMu.Lock()
		defer mihomoMu.Unlock()
	}
	node, err := c.Current(ctx, group)
	if err != nil || node == "" {
		reply("controller", err)
		return
	}
	out["node"] = node
	if nodeBankRuntimeEnabled() {
		ctx = mihomo.WithNode(ctx, node)
	}
	ip, err := c.EgressIP(ctx)
	if err != nil {
		reply("proxy_transport", err)
		return
	}
	out["egress_ip"] = ip
	out["proxy_ok"] = true

	// Freeze the existing compatibility profile for both body and headers;
	// another background refresh must not change only one half of the test.
	client := s.zen.WithHTTP(c.ProxyClient())
	profile := s.zen.CurrentGate()
	client.Gate, client.GateProvider = profile, nil
	tools, _ := zen.PrepareTools(nil, profile)
	model := proxyProbeModel
	out["model"] = model
	out["tools_injected"] = len(tools)
	body := map[string]any{
		"model":             model,
		"input":             []map[string]any{{"role": "user", "content": []map[string]any{{"type": "input_text", "text": "hi"}}}},
		"max_output_tokens": 16, "store": false, "stream": true,
	}
	if len(tools) > 0 {
		body["tools"], body["tool_choice"] = tools, "auto"
	}
	events, status, err := client.Stream(ctx, model, "proxy-test", body)
	out["status"] = status
	if err != nil {
		stage, hint := proxyProbeFailure(status, err.Error())
		out["hint"] = hint
		reply(stage, err)
		return
	}
	terminal, failed := false, false
	for event := range events {
		if event.StreamInterrupted {
			failed = true
		}
		switch event.Type {
		case "response.completed", "response.incomplete":
			terminal = true
		case "response.failed", "error":
			failed = true
		}
	}
	if !terminal || failed || ctx.Err() != nil {
		out["hint"] = "Tunnel berfungsi, tetapi stream Zen gagal atau terputus sebelum hasil akhir; belum terverifikasi."
		reply("upstream_stream", ctx.Err())
		return
	}
	out["ok"], out["zen_ok"] = true, true
	out["hint"] = "Satu request kecil diterima pada node ini; tidak menjamin seluruh model atau request berikutnya."
	reply("", nil)
}

func proxyProbeFailure(status int, detail string) (string, string) {
	switch {
	case gate.IsIdentityRejection(status, detail):
		return "upstream_access", "Zen menolak akses atau identitas klien. Ini bukan bukti limit IP; penolakan ini tidak memicu rotasi."
	case status == http.StatusForbidden:
		return "upstream_forbidden", "Proxy berhasil terhubung, tetapi upstream menolak akses (403). Periksa detail penolakan dan akses model; node tidak diganti oleh tes ini."
	case status == http.StatusUnauthorized:
		return "upstream_auth", "Upstream menolak kredensial; periksa API key dan akses model."
	case status == http.StatusTooManyRequests:
		return "upstream_quota", "Upstream membatasi request. Tunggu sesuai kebijakan penyedia; tes tidak mencoba IP lain."
	case status == 0:
		return "upstream_transport", "IP keluar terukur, tetapi koneksi menuju Zen gagal."
	default:
		return "upstream_request", "Tunnel berfungsi, tetapi request ditolak upstream; periksa model dan detail error."
	}
}
