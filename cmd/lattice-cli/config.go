package main

import (
	"flag"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"
)

const (
	// DefaultPort is the canonical default TCP port for the Lattice server.
	DefaultPort = 9099

	// DefaultHost is the canonical default loopback host.
	DefaultHost = "127.0.0.1"

	// DefaultTimeout is the default request write/read timeout.
	DefaultTimeout = 5 * time.Second

	// Version is the current semantic release version of the Lattice client.
	Version = "v1.0.0-phase12"
)

// Config holds configuration parameters for the lattice-cli REPL.
type Config struct {
	Address string
	Timeout time.Duration
	NoColor bool
}

// DefaultConfig returns default client configuration connecting to loopback.
func DefaultConfig() Config {
	return Config{
		Address: net.JoinHostPort(DefaultHost, strconv.Itoa(DefaultPort)),
		Timeout: DefaultTimeout,
		NoColor: false,
	}
}

// ParseCLIFlags parses command line arguments and populates Config.
// Returns (cfg, isHelpOrVersion, error).
func ParseCLIFlags(args []string, stdout, stderr io.Writer) (*Config, bool, error) {
	cfg := DefaultConfig()

	fs := flag.NewFlagSet("lattice-cli", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		flagAddress      string
		flagPort         int
		flagTimeout      time.Duration
		flagNoColor      bool
		flagHelp         bool
		flagHelpShort    bool
		flagVersion      bool
		flagVersionShort bool
	)

	fs.StringVar(&flagAddress, "address", "", "Lattice server address (host:port)")
	fs.IntVar(&flagPort, "port", -1, "Lattice server port override (1..65535)")
	fs.DurationVar(&flagTimeout, "timeout", cfg.Timeout, "Request timeout duration")
	fs.BoolVar(&flagNoColor, "no-color", false, "Disable colored ANSI output")
	fs.BoolVar(&flagHelp, "help", false, "Display usage instructions and exit")
	fs.BoolVar(&flagHelpShort, "h", false, "Display usage instructions and exit")
	fs.BoolVar(&flagVersion, "version", false, "Display version information and exit")
	fs.BoolVar(&flagVersionShort, "v", false, "Display version information and exit")

	fs.Usage = func() {
		fmt.Fprintf(stdout, "Lattice Interactive REPL Client (%s)\n\n", Version)
		fmt.Fprintf(stdout, "Usage:\n")
		fmt.Fprintf(stdout, "  lattice-cli [flags]\n\n")
		fmt.Fprintf(stdout, "Flags:\n")
		fmt.Fprintf(stdout, "  --address string     Server address to connect to (default %q)\n", cfg.Address)
		fmt.Fprintf(stdout, "  --port int           Server port override (1..65535)\n")
		fmt.Fprintf(stdout, "  --timeout duration   Request timeout (default %s)\n", DefaultTimeout)
		fmt.Fprintf(stdout, "  --no-color           Disable ANSI color formatting\n")
		fmt.Fprintf(stdout, "  -h, --help           Show help documentation\n")
		fmt.Fprintf(stdout, "  -v, --version        Show version information\n\n")
		fmt.Fprintf(stdout, "Commands:\n")
		fmt.Fprintf(stdout, "  PUT <key> <val>      Store a key-value pair (supports quotes and \\xHH escapes)\n")
		fmt.Fprintf(stdout, "  GET <key>            Retrieve a value by key\n")
		fmt.Fprintf(stdout, "  DELETE <key>         Remove a key\n")
		fmt.Fprintf(stdout, "  EXISTS <key>         Check key existence\n")
		fmt.Fprintf(stdout, "  STATS                Query server statistics\n")
		fmt.Fprintf(stdout, "  HELP                 Display command help\n")
		fmt.Fprintf(stdout, "  EXIT, QUIT           Exit the REPL\n")
	}

	if err := fs.Parse(args); err != nil {
		return nil, false, err
	}

	if flagHelp || flagHelpShort {
		fs.Usage()
		return &cfg, true, nil
	}

	if flagVersion || flagVersionShort {
		fmt.Fprintf(stdout, "lattice-cli %s\n", Version)
		return &cfg, true, nil
	}

	// Address resolution
	if flagAddress != "" {
		host, portStr, err := net.SplitHostPort(flagAddress)
		if err != nil {
			// If missing port, treat flagAddress as host
			host = flagAddress
			portStr = strconv.Itoa(DefaultPort)
		}
		p, err := strconv.Atoi(portStr)
		if err != nil || p < 1 || p > 65535 {
			return nil, false, fmt.Errorf("invalid port in address %q: must be between 1 and 65535", flagAddress)
		}
		cfg.Address = net.JoinHostPort(host, strconv.Itoa(p))
	}

	portSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "port" {
			portSet = true
		}
	})

	if portSet {
		if flagPort < 1 || flagPort > 65535 {
			return nil, false, fmt.Errorf("invalid port %d: must be between 1 and 65535", flagPort)
		}
		host, _, err := net.SplitHostPort(cfg.Address)
		if err != nil {
			host = DefaultHost
		}
		cfg.Address = net.JoinHostPort(host, strconv.Itoa(flagPort))
	}

	if flagTimeout <= 0 {
		return nil, false, fmt.Errorf("invalid timeout %v: must be positive", flagTimeout)
	}
	cfg.Timeout = flagTimeout

	cfg.NoColor = flagNoColor

	return &cfg, false, nil
}
