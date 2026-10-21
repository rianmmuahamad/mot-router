package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNodeBankFiveThousandSnapshotBoundary(t *testing.T) {
	dir := t.TempDir()
	nodes := []any{}
	for i := 0; i < 5000; i++ {
		nodes = append(nodes, map[string]any{"exit_ip": fmt.Sprintf("198.18.%d.%d", i/256, i%256)})
	}
	snapshot := map[string]any{"schema_version": 1, "generated_at": time.Now().Unix(), "vantage": "laptop", "zen_verified": false, "nodes": nodes}
	writeNodeBankFixture(t, dir, snapshot)
	ips, _, err := nodeBankIPs(dir)
	if err != nil || len(ips) != 5000 {
		t.Fatalf("5000-candidate snapshot rejected/truncated: %d %v", len(ips), err)
	}
	snapshot["nodes"] = append(nodes, map[string]any{"exit_ip": "198.19.0.1"})
	writeNodeBankFixture(t, dir, snapshot)
	if _, _, err := nodeBankIPs(dir); err == nil {
		t.Fatal("5001-candidate snapshot accepted")
	}
}

func writeNodeBankFixture(t *testing.T, dataDir string, snapshot any) {
	t.Helper()
	path := filepath.Join(dataDir, "nodebank", "healthy-v1.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestNodeBankIPsLoadsOnlyExitIPs(t *testing.T) {
	dir := t.TempDir()
	writeNodeBankFixture(t, dir, map[string]any{
		"schema_version": 1, "generated_at": time.Now().Unix(),
		"vantage": "laptop", "zen_verified": false,
		"nodes": []any{
			map[string]any{"exit_ip": "192.0.2.10", "config": map[string]any{"password": "must-not-leak"}},
			map[string]any{"exit_ip": "2001:db8::1", "config": map[string]any{"uuid": "must-not-leak-either"}},
		},
	})

	ips, _, err := nodeBankIPs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ips) != 2 || ips[0] != "192.0.2.10" || ips[1] != "2001:db8::1" {
		t.Fatalf("unexpected IPs: %#v", ips)
	}
}

func TestNodeBankIPsRejectsUnverifiedMetadata(t *testing.T) {
	dir := t.TempDir()
	writeNodeBankFixture(t, dir, map[string]any{
		"schema_version": 1, "generated_at": time.Now().Unix(),
		"vantage": "vps", "zen_verified": true,
		"nodes": []any{map[string]any{"exit_ip": "192.0.2.10"}},
	})
	if _, _, err := nodeBankIPs(dir); err == nil {
		t.Fatal("expected provenance validation error")
	}
}

func TestNodeBankIPsMissingSnapshotIsEmpty(t *testing.T) {
	dir := t.TempDir()
	ips, generated, err := nodeBankIPs(dir)
	if err != nil || len(ips) != 0 || generated != 0 {
		t.Fatalf("missing snapshot = (%v, %d, %v), want empty", ips, generated, err)
	}
}

func TestWithNodeBankCandidatesAttachesToMOTWithoutChangingHealth(t *testing.T) {
	tips := []string{"192.0.2.10", "2001:db8::1"}
	groups := withNodeBankCandidates([]mihomoGroupView{{Name: "MOT", Now: "current", Nodes: 4, Selectable: true}}, tips, 123)
	if len(groups) != 1 || groups[0].Nodes != 4 || len(groups[0].NodeBankIPs) != 2 {
		t.Fatalf("MOT runtime health should stay distinct from NodeBank candidates: %#v", groups)
	}
	if groups[0].NodeBankReceived != 123 || !groups[0].Selectable {
		t.Fatalf("candidate metadata missing: %#v", groups[0])
	}
}

func TestWithNodeBankCandidatesAddsNonSelectableMOTRow(t *testing.T) {
	groups := withNodeBankCandidates(nil, []string{"192.0.2.10"}, 123)
	if len(groups) != 1 || groups[0].Name != "MOT" || groups[0].Selectable || groups[0].Nodes != 0 {
		t.Fatalf("expected a pending, non-selectable MOT row: %#v", groups)
	}
}
