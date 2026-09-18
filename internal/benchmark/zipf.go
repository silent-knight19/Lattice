package benchmark

import (
	"errors"
	"fmt"
	"math"
	"math/rand"
)

const (
	// DefaultZipfTheta is the canonical Zipfian skew parameter (s = 0.99)
	// defined by the Lattice architecture and standard YCSB benchmark profiles.
	DefaultZipfTheta = 0.99

	// DefaultKeyPrefix is the default string prefix for generated benchmark keys.
	DefaultKeyPrefix = "key:"

	// MaxKeyspace defines the maximum permitted keyspace size (1 billion keys).
	MaxKeyspace uint64 = 1_000_000_000

	// MinKeyspace defines the minimum permitted keyspace size (1 key).
	MinKeyspace uint64 = 1

	// zetaExactCutoff is the threshold up to which generalized harmonic sums
	// are computed with direct summation rather than Euler-Maclaurin integration.
	zetaExactCutoff uint64 = 10_000
)

var (
	// ErrInvalidKeyspace indicates that the requested keyspace N is zero.
	ErrInvalidKeyspace = errors.New("benchmark: keyspace must be at least 1")

	// ErrKeyspaceTooLarge indicates that N exceeds MaxKeyspace (1,000,000,000).
	ErrKeyspaceTooLarge = errors.New("benchmark: keyspace exceeds maximum limit of 1,000,000,000")

	// ErrInvalidZipfTheta indicates that the skew parameter theta is outside (0.0, 1.0).
	ErrInvalidZipfTheta = errors.New("benchmark: zipfian theta must be in range (0.0, 1.0)")
)

// ZipfGenerator generates discrete key ranks and formatted keys following a
// finite Zipfian (power-law) distribution.
//
// Mathematical Model:
// For a finite keyspace of size N items (ranks r in [1, N], 0-indexed i in [0, N-1])
// and skew parameter theta in (0.0, 1.0), the probability of rank r is:
//
//	P(rank = r) = r^(-theta) / H(N, theta)
//
// where H(N, theta) is the generalized harmonic number of order theta:
//
//	H(N, theta) = sum_{j=1}^N j^(-theta)
//
// When theta = 0.99, the distribution exhibits strong non-uniformity modeling
// real-world access hotspots (e.g. the top 20% of keys account for ~75-80% of operations).
//
// Sampling Algorithm:
// Implements the Jim Gray et al. (SIGMOD 1994) / YCSB (Cooper et al., 2010)
// finite Zipfian inversion algorithm, operating in O(1) expected time per sample
// with O(1) auxiliary memory.
//
// Concurrency Model:
// ZipfGenerator is intentionally NOT concurrency-safe. Each concurrent worker
// goroutine in a benchmark runner should instantiate its own generator backed
// by an independent deterministic seed. This eliminates cross-worker mutex contention.
type ZipfGenerator struct {
	n      uint64
	theta  float64
	alpha  float64
	zeta2  float64
	zetan  float64
	eta    float64
	prefix []byte
	rng    *rand.Rand
}

// NewDefaultZipfGenerator constructs a ZipfGenerator with the canonical skew
// parameter (theta = 0.99), the default key prefix ("key:"), and the specified seed.
func NewDefaultZipfGenerator(n uint64, seed int64) (*ZipfGenerator, error) {
	return NewZipfGeneratorWithPrefix(n, DefaultZipfTheta, seed, DefaultKeyPrefix)
}

// NewZipfGenerator constructs a ZipfGenerator with a custom skew parameter theta in (0.0, 1.0)
// and the default key prefix ("key:").
func NewZipfGenerator(n uint64, theta float64, seed int64) (*ZipfGenerator, error) {
	return NewZipfGeneratorWithPrefix(n, theta, seed, DefaultKeyPrefix)
}

// NewZipfGeneratorWithPrefix constructs a ZipfGenerator with custom keyspace, skew, seed, and prefix.
func NewZipfGeneratorWithPrefix(n uint64, theta float64, seed int64, prefix string) (*ZipfGenerator, error) {
	if n < MinKeyspace {
		return nil, ErrInvalidKeyspace
	}
	if n > MaxKeyspace {
		return nil, fmt.Errorf("%w: requested %d", ErrKeyspaceTooLarge, n)
	}
	if theta <= 0.0 || theta >= 1.0 || math.IsNaN(theta) || math.IsInf(theta, 0) {
		return nil, ErrInvalidZipfTheta
	}

	alpha := 1.0 / (1.0 - theta)
	zeta2 := 1.0 + math.Pow(0.5, theta)
	zetan := computeZeta(n, theta)

	var eta float64
	if n > 1 {
		eta = (1.0 - math.Pow(2.0/float64(n), 1.0-theta)) / (1.0 - zeta2/zetan)
	}

	// Use an isolated PRNG source to prevent global RNG interference.
	src := rand.NewSource(seed)
	rng := rand.New(src)

	return &ZipfGenerator{
		n:      n,
		theta:  theta,
		alpha:  alpha,
		zeta2:  zeta2,
		zetan:  zetan,
		eta:    eta,
		prefix: []byte(prefix),
		rng:    rng,
	}, nil
}

// Next samples and returns the next 0-indexed item rank in [0, n-1].
// Rank 0 corresponds to the most frequently accessed item (the primary hotspot).
// This method executes in O(1) expected time and performs 0 heap allocations.
func (z *ZipfGenerator) Next() uint64 {
	if z.n <= 1 {
		return 0
	}

	u := z.rng.Float64()
	for u <= 0.0 || u >= 1.0 {
		u = z.rng.Float64()
	}

	uz := u * z.zetan
	if uz < 1.0 {
		return 0
	}
	if uz < z.zeta2 {
		return 1
	}

	v := float64(z.n) * math.Pow(z.eta*u-z.eta+1.0, z.alpha)
	if math.IsNaN(v) || v < 0.0 {
		return 0
	}

	idx := uint64(v)
	if idx >= z.n {
		idx = z.n - 1
	}
	return idx
}

// NextKeyBuf writes the next formatted key into dst[:0] and returns the slice.
// If cap(dst) is sufficient (at least len(prefix) + 10 bytes), this operation
// performs zero heap allocations.
func (z *ZipfGenerator) NextKeyBuf(dst []byte) []byte {
	rank := z.Next()
	return z.FormatKey(dst, rank)
}

// NextKey samples the next rank and returns a freshly allocated byte slice containing
// the formatted key. Safe for callers that need to retain the slice.
func (z *ZipfGenerator) NextKey() []byte {
	rank := z.Next()
	// len(prefix) + 10 digits
	buf := make([]byte, 0, len(z.prefix)+10)
	return z.FormatKey(buf, rank)
}

// FormatKey serializes an integer rank into dst[:0] using fixed 10-digit zero padding.
// The format is: <prefix><%010d>.
// Keys formatted with this method preserve unsigned lexicographical sorting order
// identical to numeric rank order for all ranks < 10,000,000,000.
func (z *ZipfGenerator) FormatKey(dst []byte, rank uint64) []byte {
	dst = dst[:0]
	dst = append(dst, z.prefix...)

	if rank < 10_000_000_000 {
		var digits [10]byte
		v := rank
		for i := 9; i >= 0; i-- {
			digits[i] = byte('0' + (v % 10))
			v /= 10
		}
		return append(dst, digits[:]...)
	}

	// Fallback for extremely large ranks: format full digits
	var digits [20]byte
	pos := len(digits)
	v := rank
	for v > 0 {
		pos--
		digits[pos] = byte('0' + (v % 10))
		v /= 10
	}
	return append(dst, digits[pos:]...)
}

// Keyspace returns the configured number of keys N.
func (z *ZipfGenerator) Keyspace() uint64 {
	return z.n
}

// Theta returns the configured skew parameter.
func (z *ZipfGenerator) Theta() float64 {
	return z.theta
}

// computeZeta computes the generalized harmonic sum H(n, theta) = sum_{j=1}^n j^(-theta).
// For n <= 10,000, it computes the exact sum directly.
// For n > 10,000, it uses direct summation for the first 10,000 terms and evaluates
// the remaining tail using Euler-Maclaurin integration with endpoint correction.
// This achieves relative error < 1e-10 while keeping initialization time under 1 millisecond.
func computeZeta(n uint64, theta float64) float64 {
	limit := n
	if limit > zetaExactCutoff {
		limit = zetaExactCutoff
	}

	var sum float64
	for i := uint64(1); i <= limit; i++ {
		sum += 1.0 / math.Pow(float64(i), theta)
	}

	if n <= zetaExactCutoff {
		return sum
	}

	// Tail approximation via Euler-Maclaurin summation:
	// integral_{k}^n x^(-theta) dx = (n^(1-theta) - k^(1-theta)) / (1-theta)
	// plus first-order endpoint correction: 0.5 * (n^(-theta) - k^(-theta))
	k := float64(zetaExactCutoff)
	nf := float64(n)
	integral := (math.Pow(nf, 1.0-theta) - math.Pow(k, 1.0-theta)) / (1.0 - theta)
	corr := 0.5 * (math.Pow(nf, -theta) - math.Pow(k, -theta))

	return sum + integral + corr
}
