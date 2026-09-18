package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/silent-knight19/lattice/internal/benchmark"
	"github.com/silent-knight19/lattice/internal/transport"
)

const (
	// Version is the semantic release version of the Lattice benchmarking harness.
	Version = "v1.0.0-phase13"

	// DefaultAddress is the canonical default loopback endpoint for the Lattice daemon.
	DefaultAddress = "127.0.0.1:9099"

	// DefaultWorkload is the canonical default mixed read/write profile.
	DefaultWorkload = "mixed"

	// DefaultReadRatio is the canonical 80/20 read/write proportion for mixed workloads.
	DefaultReadRatio = 0.8

	// DefaultConcurrency is the default number of concurrent worker goroutines.
	DefaultConcurrency = 64

	// DefaultDuration is the default timed benchmark window.
	DefaultDuration = 60 * time.Second

	// DefaultKeyDistribution is the default Zipfian key access distribution.
	DefaultKeyDistribution = "zipfian"

	// DefaultKeyspace is the default size of the benchmark key domain.
	DefaultKeyspace uint64 = 10_000

	// DefaultValSize is the default payload size for PUT operations (256 bytes).
	DefaultValSize = 256

	// DefaultSeed is the default PRNG seed for deterministic benchmark reproduction.
	DefaultSeed int64 = 42

	// DefaultTimeout is the default per-operation network timeout.
	DefaultTimeout = 5 * time.Second

	// DefaultPopulate indicates whether to pre-populate keyspace before timed execution.
	DefaultPopulate = true

	// DefaultPopulateCap is the maximum number of keys pre-populated by default when populate-keys is 0.
	DefaultPopulateCap uint64 = 10_000

	// MinConcurrency is the minimum permitted worker concurrency.
	MinConcurrency = 1

	// MaxConcurrency is the maximum permitted worker concurrency (bounded by server MaxConnections).
	MaxConcurrency = 1024

	// MinDuration is the minimum permitted benchmark execution duration.
	MinDuration = 100 * time.Millisecond

	// MaxDuration is the maximum permitted benchmark execution duration (24 hours).
	MaxDuration = 24 * time.Hour

	// MinValSize is the minimum permitted value size (0-byte empty value).
	MinValSize = 0

	// MaxValSize is the maximum permitted value size (4 MiB, matching protocol MaxValueLength).
	MaxValSize = int(transport.MaxValueLength)

	// MinTimeout is the minimum permitted per-operation network timeout.
	MinTimeout = 100 * time.Millisecond

	// MaxTimeout is the maximum permitted per-operation network timeout.
	MaxTimeout = 60 * time.Second
)

// WorkloadType identifies the benchmark operation mix.
type WorkloadType string

const (
	WorkloadRead  WorkloadType = "read"
	WorkloadWrite WorkloadType = "write"
	WorkloadMixed WorkloadType = "mixed"
)

// DistributionType identifies the key access distribution.
type DistributionType string

const (
	DistributionZipfian DistributionType = "zipfian"
	DistributionUniform DistributionType = "uniform"
)

// Config encapsulates validated configuration parameters for cmd/lattice-bench.
type Config struct {
	Address         string
	Workload        WorkloadType
	ReadRatio       float64
	Concurrency     int
	Duration        time.Duration
	KeyDistribution DistributionType
	Keyspace        uint64
	ValSize         int
	Seed            int64
	Timeout         time.Duration
	Populate        bool
	PopulateKeys    uint64
}

// DefaultConfig returns production-hardened defaults for the benchmark runner.
func DefaultConfig() Config {
	return Config{
		Address:         DefaultAddress,
		Workload:        WorkloadMixed,
		ReadRatio:       DefaultReadRatio,
		Concurrency:     DefaultConcurrency,
		Duration:        DefaultDuration,
		KeyDistribution: DistributionZipfian,
		Keyspace:        DefaultKeyspace,
		ValSize:         DefaultValSize,
		Seed:            DefaultSeed,
		Timeout:         DefaultTimeout,
		Populate:        DefaultPopulate,
		PopulateKeys:    DefaultKeyspace,
	}
}

// ParseFlags parses command line arguments and populates Config with validation.
// Returns (cfg, isHelpOrVersion, error).
func ParseFlags(args []string, stdout, stderr io.Writer) (*Config, bool, error) {
	cfg := DefaultConfig()

	fs := flag.NewFlagSet("lattice-bench", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		flagAddress      string
		flagWorkload     string
		flagReadRatio    float64
		flagConcurrency  int
		flagDuration     time.Duration
		flagDistribution string
		flagKeys         uint64
		flagKeyspace     uint64
		flagValSize      int
		flagSeed         int64
		flagTimeout      time.Duration
		flagPopulate     bool
		flagPopulateKeys uint64
		flagHelp         bool
		flagHelpShort    bool
		flagVersion      bool
		flagVersionShort bool
	)

	fs.StringVar(&flagAddress, "address", cfg.Address, "Lattice server endpoint (host:port)")
	fs.StringVar(&flagWorkload, "workload", string(cfg.Workload), "Workload profile: read, write, mixed")
	fs.Float64Var(&flagReadRatio, "read-ratio", cfg.ReadRatio, "Ratio of GET operations in mixed workload [0.0, 1.0]")
	fs.IntVar(&flagConcurrency, "concurrency", cfg.Concurrency, "Number of concurrent worker goroutines [1, 1024]")
	fs.DurationVar(&flagDuration, "duration", cfg.Duration, "Benchmark run duration (e.g. 60s, 5m) [100ms, 24h]")
	fs.StringVar(&flagDistribution, "key-distribution", string(cfg.KeyDistribution), "Key access distribution: zipfian, uniform")
	fs.Uint64Var(&flagKeys, "keys", cfg.Keyspace, "Keyspace domain size [1, 1000000000]")
	fs.Uint64Var(&flagKeyspace, "keyspace", 0, "Alias for --keys")
	fs.IntVar(&flagValSize, "val-size", cfg.ValSize, "Value size in bytes for PUT operations [0, 4194304]")
	fs.Int64Var(&flagSeed, "seed", cfg.Seed, "Base random seed for reproducibility")
	fs.DurationVar(&flagTimeout, "timeout", cfg.Timeout, "Per-operation network timeout [100ms, 60s]")
	fs.BoolVar(&flagPopulate, "populate", cfg.Populate, "Pre-populate keyspace before timed execution")
	fs.Uint64Var(&flagPopulateKeys, "populate-keys", 0, "Number of keys to pre-populate (0 = match keyspace up to cap)")
	fs.BoolVar(&flagHelp, "help", false, "Display usage instructions and exit")
	fs.BoolVar(&flagHelpShort, "h", false, "Display usage instructions and exit")
	fs.BoolVar(&flagVersion, "version", false, "Display version information and exit")
	fs.BoolVar(&flagVersionShort, "v", false, "Display version information and exit")

	fs.Usage = func() {
		fmt.Fprintf(stdout, "Usage: lattice-bench [options]\n\n")
		fmt.Fprintf(stdout, "High-concurrency standalone load generator and latency profiler for Lattice.\n\n")
		fmt.Fprintf(stdout, "Options:\n")
		fmt.Fprintf(stdout, "  --address string           Lattice server endpoint (default %q)\n", DefaultAddress)
		fmt.Fprintf(stdout, "  --workload string          Workload profile: read, write, mixed (default %q)\n", DefaultWorkload)
		fmt.Fprintf(stdout, "  --read-ratio float         Proportion of reads in mixed workload [0.0, 1.0] (default %.1f)\n", DefaultReadRatio)
		fmt.Fprintf(stdout, "  --concurrency int          Concurrent worker connections [1, 1024] (default %d)\n", DefaultConcurrency)
		fmt.Fprintf(stdout, "  --duration duration        Benchmark duration (default %v)\n", DefaultDuration)
		fmt.Fprintf(stdout, "  --key-distribution string  Access distribution: zipfian, uniform (default %q)\n", DefaultKeyDistribution)
		fmt.Fprintf(stdout, "  --keys uint                Keyspace domain size [1, 1000000000] (default %d)\n", DefaultKeyspace)
		fmt.Fprintf(stdout, "  --keyspace uint            Alias for --keys\n")
		fmt.Fprintf(stdout, "  --val-size int             Value size in bytes for PUT [0, 4194304] (default %d)\n", DefaultValSize)
		fmt.Fprintf(stdout, "  --seed int                 PRNG base seed for deterministic runs (default %d)\n", DefaultSeed)
		fmt.Fprintf(stdout, "  --timeout duration         Per-operation network timeout (default %v)\n", DefaultTimeout)
		fmt.Fprintf(stdout, "  --populate                 Pre-populate keyspace before timed execution (default %v)\n", DefaultPopulate)
		fmt.Fprintf(stdout, "  --populate-keys uint       Number of keys to pre-populate (0 = match keyspace up to cap %d)\n", DefaultPopulateCap)
		fmt.Fprintf(stdout, "  -h, --help                 Display usage instructions and exit\n")
		fmt.Fprintf(stdout, "  -v, --version              Display version information and exit\n")
	}

	if err := fs.Parse(args); err != nil {
		return nil, false, err
	}

	if flagHelp || flagHelpShort {
		fs.Usage()
		return nil, true, nil
	}

	if flagVersion || flagVersionShort {
		fmt.Fprintf(stdout, "lattice-bench version %s\n", Version)
		return nil, true, nil
	}

	// 1. Validate Address
	addr := strings.TrimSpace(flagAddress)
	if addr == "" {
		return nil, false, errors.New("address cannot be empty")
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, false, fmt.Errorf("invalid address format %q: expected host:port", addr)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return nil, false, fmt.Errorf("invalid port %q: must be in range 1..65535", portStr)
	}
	cfg.Address = net.JoinHostPort(host, strconv.Itoa(port))

	// 2. Validate Workload
	workloadLower := strings.ToLower(strings.TrimSpace(flagWorkload))
	switch WorkloadType(workloadLower) {
	case WorkloadRead:
		cfg.Workload = WorkloadRead
		cfg.ReadRatio = 1.0
	case WorkloadWrite:
		cfg.Workload = WorkloadWrite
		cfg.ReadRatio = 0.0
	case WorkloadMixed:
		cfg.Workload = WorkloadMixed
		// Validate ReadRatio
		if math.IsNaN(flagReadRatio) || math.IsInf(flagReadRatio, 0) || flagReadRatio < 0.0 || flagReadRatio > 1.0 {
			return nil, false, fmt.Errorf("invalid read-ratio %v: must be in range [0.0, 1.0]", flagReadRatio)
		}
		cfg.ReadRatio = flagReadRatio
	default:
		return nil, false, fmt.Errorf("unknown workload %q: must be 'read', 'write', or 'mixed'", flagWorkload)
	}

	// 3. Validate Concurrency
	if flagConcurrency < MinConcurrency || flagConcurrency > MaxConcurrency {
		return nil, false, fmt.Errorf("invalid concurrency %d: must be in range [%d, %d]", flagConcurrency, MinConcurrency, MaxConcurrency)
	}
	cfg.Concurrency = flagConcurrency

	// 4. Validate Duration
	if flagDuration < MinDuration || flagDuration > MaxDuration {
		return nil, false, fmt.Errorf("invalid duration %v: must be in range [%v, %v]", flagDuration, MinDuration, MaxDuration)
	}
	cfg.Duration = flagDuration

	// 5. Validate Key Distribution
	distLower := strings.ToLower(strings.TrimSpace(flagDistribution))
	switch DistributionType(distLower) {
	case DistributionZipfian:
		cfg.KeyDistribution = DistributionZipfian
	case DistributionUniform:
		cfg.KeyDistribution = DistributionUniform
	default:
		return nil, false, fmt.Errorf("unknown key-distribution %q: must be 'zipfian' or 'uniform'", flagDistribution)
	}

	// 6. Validate Keyspace
	keys := flagKeys
	if flagKeyspace > 0 {
		keys = flagKeyspace
	}
	if keys < benchmark.MinKeyspace || keys > benchmark.MaxKeyspace {
		return nil, false, fmt.Errorf("invalid keyspace %d: must be in range [%d, %d]", keys, benchmark.MinKeyspace, benchmark.MaxKeyspace)
	}
	cfg.Keyspace = keys

	// 7. Validate ValSize
	if flagValSize < MinValSize || flagValSize > MaxValSize {
		return nil, false, fmt.Errorf("invalid val-size %d: must be in range [%d, %d]", flagValSize, MinValSize, MaxValSize)
	}
	cfg.ValSize = flagValSize

	// 8. Validate Seed
	cfg.Seed = flagSeed

	// 9. Validate Timeout
	if flagTimeout < MinTimeout || flagTimeout > MaxTimeout {
		return nil, false, fmt.Errorf("invalid timeout %v: must be in range [%v, %v]", flagTimeout, MinTimeout, MaxTimeout)
	}
	cfg.Timeout = flagTimeout

	// 10. Populate Configuration
	cfg.Populate = flagPopulate
	if !cfg.Populate || cfg.Workload == WorkloadWrite {
		// When populate is disabled or workload is write-only, pre-population is skipped.
		if flagPopulateKeys > 0 {
			if flagPopulateKeys > cfg.Keyspace {
				return nil, false, fmt.Errorf("populate-keys (%d) cannot exceed total keyspace (%d)", flagPopulateKeys, cfg.Keyspace)
			}
			cfg.PopulateKeys = flagPopulateKeys
		} else {
			cfg.PopulateKeys = 0
		}
	} else {
		// For read and mixed workloads with Populate == true:
		// Enforce the full pre-population invariant (no silent partial datasets).
		if flagPopulateKeys > 0 {
			if flagPopulateKeys > cfg.Keyspace {
				return nil, false, fmt.Errorf("populate-keys (%d) cannot exceed total keyspace (%d)", flagPopulateKeys, cfg.Keyspace)
			}
			if flagPopulateKeys < cfg.Keyspace {
				return nil, false, fmt.Errorf("partial pre-population (%d < %d) is not permitted for %s workload: either populate the entire keyspace (--populate-keys=%d) or disable pre-population (--populate=false)", flagPopulateKeys, cfg.Keyspace, cfg.Workload, cfg.Keyspace)
			}
			cfg.PopulateKeys = flagPopulateKeys
		} else {
			// flagPopulateKeys == 0 (default):
			if cfg.Keyspace <= DefaultPopulateCap {
				cfg.PopulateKeys = cfg.Keyspace
			} else {
				return nil, false, fmt.Errorf("keyspace %d exceeds default populate cap %d: for %s workload, either explicitly specify full pre-population (--populate-keys=%d) or disable pre-population (--populate=false)", cfg.Keyspace, DefaultPopulateCap, cfg.Workload, cfg.Keyspace)
			}
		}
	}

	return &cfg, false, nil
}
