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
	"path/filepath"
	"strconv"
	"strings"

	"github.com/silent-knight19/lattice/internal/errors"
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

// Config encapsulates process and server configuration for the Lattice daemon.
type Config struct {
	DataDir           string `json:"data_dir"`
	Address           string `json:"address"`
	Port              int    `json:"port"`
	ConfigPath        string `json:"-"`
	InsecureTransport bool   `json:"insecure_transport"`
}

// DefaultConfig returns production-hardened defaults for the Lattice daemon.
func DefaultConfig() Config {
	return Config{
		DataDir:           DefaultDataDir,
		Address:           net.JoinHostPort(DefaultHost, strconv.Itoa(DefaultPort)),
		Port:              DefaultPort,
		InsecureTransport: false,
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

// Validate checks configuration invariants.
func (c *Config) Validate() error {
	if c.DataDir == "" {
		return fmt.Errorf("config error: --data-dir cannot be empty")
	}
	c.DataDir = filepath.Clean(c.DataDir)

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

	return nil
}

// loadConfigFile parses a configuration file from disk.
// Supports standard JSON or simple key-value / YAML lines with comments.
func loadConfigFile(path string) (*Config, error) {
	cleanPath := filepath.Clean(path)

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
		if err := json.Unmarshal(trimmed, &cfg); err != nil {
			return nil, fmt.Errorf("malformed JSON in config file: %w", err)
		}
		return &cfg, nil
	}

	// Line-by-line key-value / YAML-like parser
	scanner := bufio.NewScanner(bytes.NewReader(data))
	lineNum := 0
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

		switch strings.ToLower(key) {
		case "data_dir", "data-dir", "storage.data_dir":
			cfg.DataDir = val
		case "port", "server.port":
			p, err := strconv.Atoi(val)
			if err != nil {
				return nil, fmt.Errorf("invalid port value on line %d: %q", lineNum, val)
			}
			cfg.Port = p
		case "address", "listen_address", "server.listen_address", "server.address":
			cfg.Address = val
		case "insecure_transport", "insecure-transport", "server.insecure_transport":
			b, err := strconv.ParseBool(val)
			if err != nil {
				return nil, fmt.Errorf("invalid boolean value on line %d: %q", lineNum, val)
			}
			cfg.InsecureTransport = b
		default:
			// Ignore unrecognized or higher-level nesting keys (e.g. "server:", "storage:")
			if val == "" {
				continue
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return &cfg, nil
}
