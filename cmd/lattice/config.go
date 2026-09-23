package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	stdErrors "errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/silent-knight19/lattice/internal/cluster"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/security"
)

const (
	// DefaultPort is the canonical default TCP port for the Lattice server daemon.
	DefaultPort = 9099

	// DefaultHost is the canonical default loopback host.
	DefaultHost = "127.0.0.1"

	// DefaultDataDir is the default filesystem directory for database storage.
	DefaultDataDir = "./data"

	// MaxConfigFileSize bounds configuration files to 1 MiB to prevent memory exhaustion DoS.
	MaxConfigFileSize = 1024 * 1024

	// Version is the current semantic release version of the Lattice database daemon.
	Version = "v1.0.0-phase12"
)

// ClusterPeersList is a slice of PeerConfig that supports deserializing from either
// a JSON array of objects ([{"id": 1, "address": "..."}]) or a delimited string ("1=...").
type ClusterPeersList []cluster.PeerConfig

// UnmarshalJSON unmarshals either a JSON array or a delimited string of peers.
func (l *ClusterPeersList) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		*l = nil
		return nil
	}
	if trimmed[0] == '[' {
		var peers []cluster.PeerConfig
		if err := json.Unmarshal(trimmed, &peers); err != nil {
			return err
		}
		*l = peers
		return nil
	}
	var str string
	if err := json.Unmarshal(trimmed, &str); err != nil {
		return err
	}
	peers, err := cluster.ParsePeersString(str)
	if err != nil {
		return err
	}
	*l = peers
	return nil
}

// Config encapsulates process and server configuration for the Lattice daemon.
type Config struct {
	DataDir           string            `json:"data_dir"`
	Address           string            `json:"address"`
	Port              int               `json:"port"`
	ConfigPath        string            `json:"-"`
	InsecureTransport bool              `json:"insecure_transport"`
	PprofAddress      string            `json:"pprof_address"`
	MetricsAddress    string            `json:"metrics_address"`
	NodeID            uint64            `json:"node_id"`
	PeerAddress       string            `json:"peer_address"`
	ClusterPeers      ClusterPeersList  `json:"cluster_peers"`
	Topology          *cluster.Topology `json:"-"`
}

// IsClusterEnabled reports whether clustering configuration is active.
func (c *Config) IsClusterEnabled() bool {
	return c.NodeID != 0 || c.PeerAddress != "" || len(c.ClusterPeers) > 0
}

// DefaultConfig returns production-hardened defaults for the Lattice daemon.
func DefaultConfig() Config {
	return Config{
		DataDir:           DefaultDataDir,
		Address:           net.JoinHostPort(DefaultHost, strconv.Itoa(DefaultPort)),
		Port:              DefaultPort,
		InsecureTransport: false,
		NodeID:            0,
		PeerAddress:       "",
		ClusterPeers:      nil,
		Topology:          nil,
	}
}

// isLoopback reports whether the given address resolves to a local loopback interface.
func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ParseFlags parses command line arguments and applies configuration precedence:
// Defaults -> Config File -> Explicit CLI Flags.
// Returns (cfg, isHelpOrVersion, error).
func ParseFlags(args []string, stdout, stderr io.Writer) (*Config, bool, error) {
	cfg := DefaultConfig()

	fs := flag.NewFlagSet("lattice", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		flagDataDir           string
		flagPort              int
		flagAddress           string
		flagConfig            string
		flagInsecureTransport bool
		flagPprofAddress      string
		flagMetricsAddress    string
		flagNodeID            uint64
		flagPeerAddress       string
		flagClusterPeers      string
		flagHelp              bool
		flagHelpShort         bool
		flagVersion           bool
		flagVersionShort      bool
	)

	fs.StringVar(&flagDataDir, "data-dir", cfg.DataDir, "Directory path for database storage (WAL, SSTables, MANIFEST)")
	fs.IntVar(&flagPort, "port", cfg.Port, "TCP server port (0..65535)")
	fs.StringVar(&flagAddress, "address", "", "TCP bind address (e.g. 127.0.0.1:9099)")
	fs.StringVar(&flagConfig, "config", "", "Path to configuration file (JSON or key-value)")
	fs.BoolVar(&flagInsecureTransport, "insecure-transport", false, "Explicit opt-in permitting unencrypted plaintext TCP on non-loopback addresses")
	fs.StringVar(&flagPprofAddress, "pprof-address", "", "TCP bind address for HTTP pprof profiling diagnostics (e.g. 127.0.0.1:6060, loopback only)")
	fs.StringVar(&flagMetricsAddress, "metrics-address", "", "TCP bind address for Prometheus metrics HTTP endpoint (e.g. 127.0.0.1:9100, :9100)")
	fs.Uint64Var(&flagNodeID, "node-id", 0, "Cluster node ID (> 0 in cluster mode; 0 for single-node)")
	fs.StringVar(&flagPeerAddress, "peer-address", "", "TCP bind address for Raft peer transport (e.g. 127.0.0.1:9098)")
	fs.StringVar(&flagClusterPeers, "cluster-peers", "", "Comma-separated list of cluster peers (format: id=host:port, e.g. 1=10.0.0.1:9098,2=10.0.0.2:9098)")
	fs.BoolVar(&flagHelp, "help", false, "Display usage instructions and exit")
	fs.BoolVar(&flagHelpShort, "h", false, "Display usage instructions and exit")
	fs.BoolVar(&flagVersion, "version", false, "Display version information and exit")
	fs.BoolVar(&flagVersionShort, "v", false, "Display version information and exit")

	fs.Usage = func() {
		fmt.Fprintf(stdout, "Usage of lattice:\n")
		fmt.Fprintf(stdout, "  lattice [flags]\n")
		fmt.Fprintf(stdout, "  lattice <command> [arguments]\n\n")
		fmt.Fprintf(stdout, "Commands:\n")
		fmt.Fprintf(stdout, "  inspect-sstable  Inspect physical SSTable file\n")
		fmt.Fprintf(stdout, "  dump-wal         Forensic dump of WAL segment file\n\n")
		fmt.Fprintf(stdout, "Flags:\n")
		fs.SetOutput(stdout)
		fs.PrintDefaults()
		fs.SetOutput(stderr)
	}

	if err := fs.Parse(args); err != nil {
		if stdErrors.Is(err, flag.ErrHelp) {
			return nil, true, nil
		}
		return nil, false, err
	}

	if flagHelp || flagHelpShort {
		fs.Usage()
		return nil, true, nil
	}

	if flagVersion || flagVersionShort {
		fmt.Fprintf(stdout, "lattice server daemon version %s\n", Version)
		return nil, true, nil
	}

	// Track which CLI flags were explicitly provided on the command line
	provided := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) {
		provided[f.Name] = true
	})

	// Step 1: Load config file if specified
	if provided["config"] {
		if flagConfig == "" {
			return nil, false, fmt.Errorf("config error: --config flag specified with empty path")
		}
		fileCfg, err := loadConfigFile(flagConfig)
		if err != nil {
			return nil, false, fmt.Errorf("config error: failed to load %s: %w", flagConfig, err)
		}
		cfg = *fileCfg
		cfg.ConfigPath = flagConfig
	}

	// Step 2: Override with explicit CLI flags
	if provided["data-dir"] {
		cfg.DataDir = flagDataDir
	}
	if provided["port"] {
		cfg.Port = flagPort
	}
	if provided["address"] {
		cfg.Address = flagAddress
	}
	if provided["insecure-transport"] {
		cfg.InsecureTransport = flagInsecureTransport
	}
	if provided["pprof-address"] {
		cfg.PprofAddress = flagPprofAddress
	}
	if provided["metrics-address"] {
		cfg.MetricsAddress = flagMetricsAddress
	}
	if provided["node-id"] {
		cfg.NodeID = flagNodeID
	}
	if provided["peer-address"] {
		cfg.PeerAddress = flagPeerAddress
	}
	if provided["cluster-peers"] {
		peers, err := cluster.ParsePeersString(flagClusterPeers)
		if err != nil {
			return nil, false, fmt.Errorf("config error: invalid --cluster-peers: %w", err)
		}
		cfg.ClusterPeers = peers
	}

	// Step 3: Normalize Address and Port
	if provided["address"] && !provided["port"] {
		// Extract port from address
		_, portStr, err := net.SplitHostPort(cfg.Address)
		if err != nil {
			return nil, false, fmt.Errorf("config error: invalid --address format %q (expected host:port): %w", cfg.Address, err)
		}
		p, err := strconv.Atoi(portStr)
		if err != nil {
			return nil, false, fmt.Errorf("config error: invalid port in --address %q: %w", cfg.Address, err)
		}
		cfg.Port = p
	} else if provided["port"] && !provided["address"] {
		// Use default loopback host with explicit port
		cfg.Address = net.JoinHostPort(DefaultHost, strconv.Itoa(cfg.Port))
	} else if provided["port"] && provided["address"] {
		// Both set: verify consistency or update host
		host, _, err := net.SplitHostPort(cfg.Address)
		if err != nil {
			host = cfg.Address
		}
		cfg.Address = net.JoinHostPort(host, strconv.Itoa(cfg.Port))
	} else if !provided["address"] && !provided["port"] && cfg.Address == "" {
		cfg.Address = net.JoinHostPort(DefaultHost, strconv.Itoa(cfg.Port))
	}

	// Normalize MetricsAddress if configured (default port without host binds to DefaultHost)
	if cfg.MetricsAddress != "" {
		if strings.HasPrefix(cfg.MetricsAddress, ":") {
			cfg.MetricsAddress = net.JoinHostPort(DefaultHost, strings.TrimPrefix(cfg.MetricsAddress, ":"))
		} else if !strings.Contains(cfg.MetricsAddress, ":") {
			cfg.MetricsAddress = net.JoinHostPort(DefaultHost, cfg.MetricsAddress)
		}
	}

	// Reject unexpected positional arguments (e.g. typos like 'lattice dump_wal' or unknown subcommands)
	if len(fs.Args()) > 0 {
		return nil, false, fmt.Errorf("config error: unexpected argument %q (see --help for usage)", fs.Args()[0])
	}

	// Step 4: Validate Normalized Configuration
	if err := cfg.Validate(); err != nil {
		return nil, false, err
	}

	return &cfg, false, nil
}

// portsCollide reports whether two host:port endpoints will collide upon binding.
func portsCollide(addr1, addr2 string) bool {
	h1, p1, err1 := net.SplitHostPort(addr1)
	h2, p2, err2 := net.SplitHostPort(addr2)
	if err1 != nil || err2 != nil {
		return false
	}
	if p1 == "0" || p2 == "0" || p1 != p2 {
		return false
	}
	if h1 == h2 {
		return true
	}
	isWildcard := func(h string) bool {
		if h == "0.0.0.0" || h == "::" || h == "[::]" {
			return true
		}
		ip := net.ParseIP(h)
		return ip != nil && ip.IsUnspecified()
	}
	if isWildcard(h1) || isWildcard(h2) {
		return true
	}
	if isLoopback(h1) && isLoopback(h2) {
		return true
	}
	ip1 := net.ParseIP(h1)
	ip2 := net.ParseIP(h2)
	if ip1 != nil && ip2 != nil {
		if ip1.Equal(ip2) {
			return true
		}
		if ip1.To4() != nil && ip2.To4() != nil && ip1.To4().Equal(ip2.To4()) {
			return true
		}
	}
	return false
}

// Validate checks configuration invariants.
func (c *Config) Validate() error {
	if c.DataDir == "" {
		return fmt.Errorf("config error: --data-dir cannot be empty")
	}
	cleanDataDir, err := security.CleanAndValidatePath(c.DataDir)
	if err != nil {
		return fmt.Errorf("config error: invalid --data-dir path: %w", err)
	}
	c.DataDir = cleanDataDir

	if c.Port < 0 || c.Port > 65535 {
		return fmt.Errorf("config error: invalid port %d (must be between 0 and 65535)", c.Port)
	}

	host, _, err := net.SplitHostPort(c.Address)
	if err != nil {
		return fmt.Errorf("config error: invalid address %q (expected host:port): %w", c.Address, err)
	}

	// Phase 11 Loopback Security Policy (SEC-P11-001):
	// Unencrypted plaintext TCP is prohibited on non-loopback addresses without explicit opt-in.
	if !c.InsecureTransport && !isLoopback(host) {
		return errors.ErrInsecureTransport
	}

	// Phase 13 M04 Pprof Security Policy:
	// Pprof profiling HTTP server must bind strictly to a loopback address.
	// InsecureTransport opt-in DOES NOT permit non-loopback pprof endpoints.
	if c.PprofAddress != "" {
		pHost, pPortStr, err := net.SplitHostPort(c.PprofAddress)
		if err != nil {
			return fmt.Errorf("config error: invalid --pprof-address %q (expected host:port): %w", c.PprofAddress, err)
		}
		pPort, err := strconv.Atoi(pPortStr)
		if err != nil || pPort < 0 || pPort > 65535 {
			return fmt.Errorf("config error: invalid port in --pprof-address %q (must be between 0 and 65535)", c.PprofAddress)
		}
		if !isLoopback(pHost) {
			return fmt.Errorf("config error: --pprof-address %q must resolve to a local loopback interface (127.0.0.1, ::1, localhost)", c.PprofAddress)
		}

		// Prevent port conflict between storage server and pprof HTTP server when both use the same port > 0
		if pPort != 0 && portsCollide(c.PprofAddress, c.Address) {
			_, srvPortStr, _ := net.SplitHostPort(c.Address)
			return fmt.Errorf("config error: --pprof-address port %s conflicts with server --address port %s", pPortStr, srvPortStr)
		}
	}

	// Phase 20 M01 Metrics Security Policy:
	if c.MetricsAddress != "" {
		mHost, mPortStr, err := net.SplitHostPort(c.MetricsAddress)
		if err != nil {
			return fmt.Errorf("config error: invalid --metrics-address %q (expected host:port): %w", c.MetricsAddress, err)
		}
		mPort, err := strconv.Atoi(mPortStr)
		if err != nil || mPort < 0 || mPort > 65535 {
			return fmt.Errorf("config error: invalid port in --metrics-address %q (must be between 0 and 65535)", c.MetricsAddress)
		}
		if !c.InsecureTransport && !isLoopback(mHost) {
			return fmt.Errorf("config error: --metrics-address %q requires --insecure-transport for non-loopback interfaces: %w", c.MetricsAddress, errors.ErrInsecureTransport)
		}
		if mPort != 0 {
			if portsCollide(c.MetricsAddress, c.Address) {
				_, srvPortStr, _ := net.SplitHostPort(c.Address)
				return fmt.Errorf("config error: --metrics-address port %s conflicts with server --address port %s", mPortStr, srvPortStr)
			}
			if c.PprofAddress != "" && portsCollide(c.MetricsAddress, c.PprofAddress) {
				_, pPortStr, _ := net.SplitHostPort(c.PprofAddress)
				return fmt.Errorf("config error: --metrics-address port %s conflicts with pprof address port %s", mPortStr, pPortStr)
			}
		}
	}

	// Phase 14 M01 Cluster Topology Policy:
	// If any cluster parameter is provided, validate cluster configuration.
	// Single-node V1 operation remains active when unconfigured.
	if c.IsClusterEnabled() {
		if c.NodeID == 0 {
			return fmt.Errorf("config error: --node-id must be greater than zero when clustering is enabled")
		}
		topo, err := cluster.NewTopology(cluster.NodeID(c.NodeID), c.PeerAddress, c.ClusterPeers)
		if err != nil {
			return fmt.Errorf("config error: invalid cluster topology: %w", err)
		}
		c.Topology = topo

		// Port collision prevention between peer listener and storage/pprof servers
		if c.Topology.LocalAddress() != "" {
			if portsCollide(c.Topology.LocalAddress(), c.Address) {
				_, peerPortStr, _ := net.SplitHostPort(c.Topology.LocalAddress())
				_, srvPortStr, _ := net.SplitHostPort(c.Address)
				return fmt.Errorf("config error: peer address port %s conflicts with server address port %s", peerPortStr, srvPortStr)
			}
			if c.PprofAddress != "" && portsCollide(c.Topology.LocalAddress(), c.PprofAddress) {
				_, peerPortStr, _ := net.SplitHostPort(c.Topology.LocalAddress())
				_, pPortStr, _ := net.SplitHostPort(c.PprofAddress)
				return fmt.Errorf("config error: peer address port %s conflicts with pprof address port %s", peerPortStr, pPortStr)
			}
			if c.MetricsAddress != "" && portsCollide(c.Topology.LocalAddress(), c.MetricsAddress) {
				_, peerPortStr, _ := net.SplitHostPort(c.Topology.LocalAddress())
				_, mPortStr, _ := net.SplitHostPort(c.MetricsAddress)
				return fmt.Errorf("config error: peer address port %s conflicts with metrics address port %s", peerPortStr, mPortStr)
			}
		}
	}

	return nil
}

// loadConfigFile parses a configuration file from disk.
// Supports standard JSON or simple key-value / YAML lines with comments.
func loadConfigFile(path string) (*Config, error) {
	cleanPath, err := security.CleanAndValidatePath(path)
	if err != nil {
		return nil, fmt.Errorf("invalid config file path: %w", err)
	}

	f, err := os.Open(cleanPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%s is a directory, not a configuration file", cleanPath)
	}
	if info.Size() > MaxConfigFileSize {
		return nil, fmt.Errorf("configuration file size %d exceeds maximum limit %d bytes", info.Size(), MaxConfigFileSize)
	}

	data, err := io.ReadAll(io.LimitReader(f, MaxConfigFileSize+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > MaxConfigFileSize {
		return nil, fmt.Errorf("configuration file size exceeds maximum limit %d bytes", MaxConfigFileSize)
	}

	cfg := DefaultConfig()

	// Attempt JSON parsing first
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		dec := json.NewDecoder(bytes.NewReader(trimmed))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&cfg); err != nil {
			return nil, fmt.Errorf("malformed JSON in config file: %w", err)
		}
		return &cfg, nil
	}

	// Line-by-line key-value / YAML-like parser
	scanner := bufio.NewScanner(bytes.NewReader(data))
	lineNum := 0
	seenKeys := make(map[string]int)
	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}

		// Split on ':' or '='
		var key, val string
		if idx := strings.IndexAny(line, ":="); idx != -1 {
			key = strings.TrimSpace(line[:idx])
			val = strings.TrimSpace(line[idx+1:])
		} else {
			return nil, fmt.Errorf("syntax error on line %d: expected key: value", lineNum)
		}

		val = strings.Trim(val, "\"'")

		var canonicalKey string
		switch strings.ToLower(key) {
		case "data_dir", "data-dir", "storage.data_dir":
			canonicalKey = "data_dir"
			cfg.DataDir = val
		case "port", "server.port":
			canonicalKey = "port"
			p, err := strconv.Atoi(val)
			if err != nil {
				return nil, fmt.Errorf("invalid port value on line %d: %q", lineNum, val)
			}
			cfg.Port = p
		case "address", "listen_address", "server.listen_address", "server.address":
			canonicalKey = "address"
			cfg.Address = val
		case "insecure_transport", "insecure-transport", "server.insecure_transport":
			canonicalKey = "insecure_transport"
			b, err := strconv.ParseBool(val)
			if err != nil {
				return nil, fmt.Errorf("invalid boolean value on line %d: %q", lineNum, val)
			}
			cfg.InsecureTransport = b
		case "pprof_address", "pprof-address", "server.pprof_address", "server.pprof-address":
			canonicalKey = "pprof_address"
			cfg.PprofAddress = val
		case "metrics_address", "metrics-address", "server.metrics_address", "server.metrics-address", "monitoring.metrics_address":
			canonicalKey = "metrics_address"
			cfg.MetricsAddress = val
		case "node_id", "node-id", "raft.node_id", "cluster.node_id":
			canonicalKey = "node_id"
			id, err := strconv.ParseUint(val, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("invalid node_id value on line %d: %q", lineNum, val)
			}
			cfg.NodeID = id
		case "peer_address", "peer-address", "raft.peer_address", "cluster.peer_address":
			canonicalKey = "peer_address"
			cfg.PeerAddress = val
		case "cluster_peers", "cluster-peers", "raft.cluster_peers", "cluster.cluster_peers":
			canonicalKey = "cluster_peers"
			peers, err := cluster.ParsePeersString(val)
			if err != nil {
				return nil, fmt.Errorf("invalid cluster_peers on line %d: %w", lineNum, err)
			}
			cfg.ClusterPeers = peers
		default:
			// Allow structural section headers without values (e.g. "server:", "storage:", "cluster:", "raft:")
			if val == "" {
				continue
			}
			return nil, fmt.Errorf("unknown configuration key on line %d: %q", lineNum, key)
		}

		if prevLine, seen := seenKeys[canonicalKey]; seen {
			return nil, fmt.Errorf("duplicate or conflicting configuration key on line %d: %q (previously defined on line %d)", lineNum, key, prevLine)
		}
		seenKeys[canonicalKey] = lineNum
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return &cfg, nil
}
