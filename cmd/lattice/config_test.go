package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/silent-knight19/lattice/internal/cluster"
)

// TestConfig_SingleNodeDefault verifies that absent cluster flags maintain V1 single-node mode.
func TestConfig_SingleNodeDefault(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cfg, isHelp, err := ParseFlags([]string{}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if isHelp {
		t.Fatal("expected isHelp to be false")
	}
	if cfg.IsClusterEnabled() {
		t.Error("expected cluster mode to be disabled by default")
	}
	if cfg.Topology != nil {
		t.Errorf("expected Topology to be nil in single-node mode, got %+v", cfg.Topology)
	}
	if cfg.NodeID != 0 {
		t.Errorf("expected NodeID 0, got %d", cfg.NodeID)
	}
}

// TestConfig_ClusterCLIFlags verifies parsing cluster topology from explicit CLI flags.
func TestConfig_ClusterCLIFlags(t *testing.T) {
	var stdout, stderr bytes.Buffer
	args := []string{
		"--node-id", "1",
		"--peer-address", "127.0.0.1:9098",
		"--cluster-peers", "1=127.0.0.1:9098,2=127.0.0.1:9097,3=127.0.0.1:9096",
	}
	cfg, _, err := ParseFlags(args, &stdout, &stderr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.IsClusterEnabled() {
		t.Fatal("expected cluster mode to be enabled")
	}
	if cfg.NodeID != 1 {
		t.Errorf("expected NodeID 1, got %d", cfg.NodeID)
	}
	if cfg.Topology == nil {
		t.Fatal("expected Topology to be non-nil")
	}
	if cfg.Topology.LocalID() != 1 {
		t.Errorf("LocalID() = %d, want 1", cfg.Topology.LocalID())
	}
	if cfg.Topology.Size() != 3 {
		t.Errorf("Size() = %d, want 3", cfg.Topology.Size())
	}
	if cfg.Topology.RemoteSize() != 2 {
		t.Errorf("RemoteSize() = %d, want 2", cfg.Topology.RemoteSize())
	}
}

// TestConfig_ClusterJSONConfig_ArrayFormat verifies loading cluster peers from a JSON array.
func TestConfig_ClusterJSONConfig_ArrayFormat(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "lattice-cluster.json")

	content := `{
		"data_dir": "` + filepath.Join(tempDir, "data") + `",
		"address": "127.0.0.1:9099",
		"node_id": 2,
		"peer_address": "127.0.0.1:9098",
		"cluster_peers": [
			{"id": 1, "address": "127.0.0.1:9097"},
			{"id": 2, "address": "127.0.0.1:9098"}
		]
	}`
	if err := os.WriteFile(configPath, []byte(content), 0600); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	var stdout, stderr bytes.Buffer
	cfg, _, err := ParseFlags([]string{"--config", configPath}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.IsClusterEnabled() {
		t.Fatal("expected cluster mode enabled from JSON")
	}
	if cfg.Topology == nil {
		t.Fatal("expected non-nil Topology")
	}
	if cfg.Topology.LocalID() != 2 {
		t.Errorf("LocalID = %d, want 2", cfg.Topology.LocalID())
	}
	if cfg.Topology.Size() != 2 {
		t.Errorf("Size = %d, want 2", cfg.Topology.Size())
	}
}

// TestConfig_ClusterJSONConfig_StringFormat verifies loading cluster peers from a JSON string.
func TestConfig_ClusterJSONConfig_StringFormat(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "lattice-cluster-str.json")

	content := `{
		"data_dir": "` + filepath.Join(tempDir, "data") + `",
		"address": "127.0.0.1:9099",
		"node_id": 1,
		"peer_address": "127.0.0.1:9098",
		"cluster_peers": "1=127.0.0.1:9098, 2=127.0.0.1:9097"
	}`
	if err := os.WriteFile(configPath, []byte(content), 0600); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	var stdout, stderr bytes.Buffer
	cfg, _, err := ParseFlags([]string{"--config", configPath}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.IsClusterEnabled() {
		t.Fatal("expected cluster mode enabled from JSON string")
	}
	if cfg.Topology.Size() != 2 {
		t.Errorf("Size = %d, want 2", cfg.Topology.Size())
	}
}

// TestConfig_ClusterKeyValConfig verifies parsing cluster topology from key-value lines.
func TestConfig_ClusterKeyValConfig(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "lattice-cluster.conf")

	content := `
# Cluster Configuration
data_dir = ` + filepath.Join(tempDir, "data") + `
address = 127.0.0.1:9099
node_id = 3
peer_address = 127.0.0.1:9098
cluster_peers = 1=127.0.0.1:9096, 2=127.0.0.1:9097, 3=127.0.0.1:9098
`
	if err := os.WriteFile(configPath, []byte(content), 0600); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	var stdout, stderr bytes.Buffer
	cfg, _, err := ParseFlags([]string{"--config", configPath}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.IsClusterEnabled() {
		t.Fatal("expected cluster mode enabled from key-value config")
	}
	if cfg.Topology.LocalID() != 3 {
		t.Errorf("LocalID = %d, want 3", cfg.Topology.LocalID())
	}
	if cfg.Topology.Size() != 3 {
		t.Errorf("Size = %d, want 3", cfg.Topology.Size())
	}
}

// TestConfig_Precedence_CLIOverridesFile verifies that CLI flags override config file values.
func TestConfig_Precedence_CLIOverridesFile(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "lattice.conf")

	content := `
node_id = 1
peer_address = 127.0.0.1:9098
cluster_peers = 1=127.0.0.1:9098, 2=127.0.0.1:9097
`
	if err := os.WriteFile(configPath, []byte(content), 0600); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	var stdout, stderr bytes.Buffer
	// CLI overrides node-id to 2 and peer-address to 127.0.0.1:9097
	args := []string{
		"--config", configPath,
		"--node-id", "2",
		"--peer-address", "127.0.0.1:9097",
	}
	cfg, _, err := ParseFlags(args, &stdout, &stderr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Topology.LocalID() != 2 {
		t.Errorf("expected overridden LocalID 2, got %d", cfg.Topology.LocalID())
	}
	if cfg.Topology.LocalAddress() != "127.0.0.1:9097" {
		t.Errorf("expected overridden LocalAddress 127.0.0.1:9097, got %s", cfg.Topology.LocalAddress())
	}
}

// TestConfig_ClusterValidationRejections verifies fail-closed behavior on invalid cluster configurations.
func TestConfig_ClusterValidationRejections(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		errSubstr string
	}{
		{
			name: "node ID zero when clustering enabled",
			args: []string{
				"--node-id", "0",
				"--peer-address", "127.0.0.1:9098",
				"--cluster-peers", "1=127.0.0.1:9098",
			},
			errSubstr: "--node-id must be greater than zero",
		},
		{
			name: "duplicate peer ID in flag",
			args: []string{
				"--node-id", "1",
				"--peer-address", "127.0.0.1:9098",
				"--cluster-peers", "1=127.0.0.1:9098, 1=127.0.0.1:9097",
			},
			errSubstr: "duplicate node ID",
		},
		{
			name: "duplicate peer address in flag",
			args: []string{
				"--node-id", "1",
				"--peer-address", "127.0.0.1:9098",
				"--cluster-peers", "1=127.0.0.1:9098, 2=127.0.0.1:9098",
			},
			errSubstr: "duplicate peer address",
		},
		{
			name: "peer port conflicts with storage server port",
			args: []string{
				"--port", "9099",
				"--node-id", "1",
				"--peer-address", "127.0.0.1:9099",
				"--cluster-peers", "1=127.0.0.1:9099",
			},
			errSubstr: "conflicts with server address port",
		},
		{
			name: "peer port conflicts with pprof server port",
			args: []string{
				"--pprof-address", "127.0.0.1:6060",
				"--node-id", "1",
				"--peer-address", "127.0.0.1:6060",
				"--cluster-peers", "1=127.0.0.1:6060",
			},
			errSubstr: "conflicts with pprof address port",
		},
		{
			name: "wildcard peer address rejected",
			args: []string{
				"--node-id", "1",
				"--peer-address", "127.0.0.1:9098",
				"--cluster-peers", "1=127.0.0.1:9098, 2=0.0.0.0:9097",
			},
			errSubstr: "wildcard IP address",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			_, _, err := ParseFlags(tt.args, &stdout, &stderr)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.errSubstr)
			}
			if !strings.Contains(err.Error(), tt.errSubstr) {
				t.Errorf("error = %q, want substring %q", err.Error(), tt.errSubstr)
			}
		})
	}
}

// FuzzParseFlags tests that ParseFlags never panics or crashes on arbitrary CLI strings.
func FuzzParseFlags(f *testing.F) {
	seeds := [][]string{
		{},
		{"--node-id", "1"},
		{"--node-id", "0"},
		{"--cluster-peers", "1=127.0.0.1:9098"},
		{"--cluster-peers", "invalid"},
		{"--peer-address", "127.0.0.1:9098"},
		{"--port", "9099"},
		{"--help"},
		{"--version"},
	}
	for _, s := range seeds {
		if len(s) > 0 {
			f.Add(strings.Join(s, " "))
		}
	}

	f.Fuzz(func(t *testing.T, cmdLine string) {
		args := strings.Fields(cmdLine)
		var stdout, stderr bytes.Buffer
		// Invariant: Must never panic
		cfg, _, err := ParseFlags(args, &stdout, &stderr)
		if err == nil && cfg != nil {
			if cfg.IsClusterEnabled() {
				if cfg.Topology == nil {
					t.Fatalf("cluster enabled but Topology is nil")
				}
				if cfg.Topology.LocalID() == cluster.NodeIDNil {
					t.Fatalf("cluster enabled but LocalID is 0")
				}
			}
		}
	})
}

func TestConfigFile_UnknownKeysRejection(t *testing.T) {
	t.Run("JSON with unknown key fails closed", func(t *testing.T) {
		tmpDir := t.TempDir()
		cfgPath := filepath.Join(tmpDir, "unknown.json")
		if err := os.WriteFile(cfgPath, []byte(`{"port": 9099, "unknown_opt": "fail"}`), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := loadConfigFile(cfgPath)
		if err == nil {
			t.Fatal("expected error on unknown JSON key, got nil")
		}
		if !strings.Contains(err.Error(), "unknown field") && !strings.Contains(err.Error(), "malformed JSON") {
			t.Errorf("unexpected error message: %v", err)
		}
	})

	t.Run("key-value with unknown key fails closed", func(t *testing.T) {
		tmpDir := t.TempDir()
		cfgPath := filepath.Join(tmpDir, "unknown.conf")
		if err := os.WriteFile(cfgPath, []byte("port: 9099\ninsecure_transprot: true\n"), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := loadConfigFile(cfgPath)
		if err == nil {
			t.Fatal("expected error on unknown key-value key, got nil")
		}
		if !strings.Contains(err.Error(), "unknown configuration key") {
			t.Errorf("unexpected error message: %v", err)
		}
	})

	t.Run("structural headers without values succeed", func(t *testing.T) {
		tmpDir := t.TempDir()
		cfgPath := filepath.Join(tmpDir, "valid_sections.conf")
		content := "server:\nport: 9099\ncluster:\nnode_id: 1\npeer_address: 127.0.0.1:9098\ncluster_peers: 1=127.0.0.1:9098\n"
		if err := os.WriteFile(cfgPath, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		cfg, err := loadConfigFile(cfgPath)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.Port != 9099 || cfg.NodeID != 1 {
			t.Fatalf("unexpected config values: %+v", cfg)
		}
	})
}

func TestConfigFile_DuplicateConflictingKeysRejection(t *testing.T) {
	t.Run("duplicate exact key fails closed", func(t *testing.T) {
		tmpDir := t.TempDir()
		cfgPath := filepath.Join(tmpDir, "dup.conf")
		content := "node_id: 1\nnode_id: 2\n"
		if err := os.WriteFile(cfgPath, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := loadConfigFile(cfgPath)
		if err == nil {
			t.Fatal("expected error on duplicate key, got nil")
		}
		if !strings.Contains(err.Error(), "duplicate or conflicting configuration key") {
			t.Errorf("unexpected error message: %v", err)
		}
	})

	t.Run("conflicting alias key fails closed", func(t *testing.T) {
		tmpDir := t.TempDir()
		cfgPath := filepath.Join(tmpDir, "conflict.conf")
		content := "node_id: 1\ncluster.node_id: 2\n"
		if err := os.WriteFile(cfgPath, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := loadConfigFile(cfgPath)
		if err == nil {
			t.Fatal("expected error on conflicting alias, got nil")
		}
		if !strings.Contains(err.Error(), "duplicate or conflicting configuration key") {
			t.Errorf("unexpected error message: %v", err)
		}
	})
}

func TestConfig_WildcardPortCollision(t *testing.T) {
	t.Run("server wildcard 0.0.0.0:9099 collides with peer 127.0.0.1:9099", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.Address = "0.0.0.0:9099"
		cfg.InsecureTransport = true
		cfg.NodeID = 1
		cfg.PeerAddress = "127.0.0.1:9099"
		cfg.ClusterPeers = []cluster.PeerConfig{{ID: 1, Address: "127.0.0.1:9099"}}

		err := cfg.Validate()
		if err == nil {
			t.Fatal("expected port collision error between wildcard server and peer, got nil")
		}
		if !strings.Contains(err.Error(), "conflicts with server address port") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("server wildcard 0.0.0.0:9099 collides with pprof 127.0.0.1:9099", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.Address = "0.0.0.0:9099"
		cfg.InsecureTransport = true
		cfg.PprofAddress = "127.0.0.1:9099"

		err := cfg.Validate()
		if err == nil {
			t.Fatal("expected port collision error between wildcard server and pprof, got nil")
		}
		if !strings.Contains(err.Error(), "conflicts with server --address port") {
			t.Errorf("unexpected error: %v", err)
		}
	})
}
