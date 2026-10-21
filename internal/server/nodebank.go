package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
)

const maxNodeBankSnapshotBytes = 32 << 20

type nodeBankSnapshot struct {
	SchemaVersion int    `json:"schema_version"`
	GeneratedAt   int64  `json:"generated_at"`
	Vantage       string `json:"vantage"`
	ZenVerified   bool   `json:"zen_verified"`
	Nodes         []struct {
		ExitIP string `json:"exit_ip"`
	} `json:"nodes"`
}

// nodeBankIPs reads only public egress addresses from the laptop's private
// snapshot. Proxy credentials/configurations never enter the dashboard API.
func nodeBankIPs(dataDir string) ([]string, int64, error) {
	if dataDir == "" {
		return nil, 0, nil
	}
	path := filepath.Join(dataDir, "nodebank", "healthy-v1.json")
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("open NodeBank snapshot: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, 0, fmt.Errorf("stat NodeBank snapshot: %w", err)
	}
	if info.Size() <= 0 || info.Size() > maxNodeBankSnapshotBytes {
		return nil, 0, fmt.Errorf("NodeBank snapshot size is invalid")
	}
	decoder := json.NewDecoder(io.LimitReader(file, maxNodeBankSnapshotBytes+1))
	var snapshot nodeBankSnapshot
	if err := decoder.Decode(&snapshot); err != nil {
		return nil, 0, fmt.Errorf("decode NodeBank snapshot: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, 0, fmt.Errorf("NodeBank snapshot has trailing data")
	}
	if snapshot.SchemaVersion != 1 || snapshot.Vantage != "laptop" || snapshot.ZenVerified || len(snapshot.Nodes) > 5000 {
		return nil, 0, fmt.Errorf("NodeBank snapshot metadata is invalid")
	}
	ips := make([]string, 0, len(snapshot.Nodes))
	seen := make(map[string]struct{}, len(snapshot.Nodes))
	for _, node := range snapshot.Nodes {
		ip := net.ParseIP(node.ExitIP)
		if ip == nil {
			return nil, 0, fmt.Errorf("NodeBank snapshot contains an invalid exit IP")
		}
		canonical := ip.String()
		if _, ok := seen[canonical]; ok {
			continue
		}
		seen[canonical] = struct{}{}
		ips = append(ips, canonical)
	}
	return ips, snapshot.GeneratedAt, nil
}
