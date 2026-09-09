package memtable

import (
	cryptorand "crypto/rand"
	"sync"
	"time"

	"github.com/silent-knight19/lattice/internal/binary"
)

// RandomSource provides pseudo-random 32-bit unsigned integers for SkipList height generation.
type RandomSource interface {
	// Uint32 returns a uniformly distributed pseudo-random 32-bit unsigned integer in [0, 2^32-1].
	Uint32() uint32
}

// PCG32 is a fast, statistically robust pseudo-random number generator implementing
// the PCG-XSH-RR algorithm (64-bit state, 32-bit output).
//
// It provides uniform bit distributions for SkipList tower height generation without
// external dependencies or the overhead of cryptographic entropy sources.
//
// Note: PCG32 is algorithmic pseudo-randomness for data structure performance and is NOT
// cryptographically secure. It must not be used for security tokens or cryptographic keys.
type PCG32 struct {
	state uint64
	inc   uint64
}

// NewPCG32 creates a new PCG32 generator with the given initial state and stream selector sequence.
func NewPCG32(initState, initSeq uint64) *PCG32 {
	p := &PCG32{
		inc: (initSeq << 1) | 1,
	}
	p.state = 0
	p.Uint32()
	p.state += initState
	p.Uint32()
	return p
}

// Uint32 returns a pseudo-random 32-bit unsigned integer in [0, 2^32-1].
func (p *PCG32) Uint32() uint32 {
	oldState := p.state
	p.state = oldState*6364136223846793005 + p.inc
	xorshifted := uint32(((oldState >> 18) ^ oldState) >> 27)
	rot := uint32(oldState >> 59)
	return (xorshifted >> rot) | (xorshifted << ((-rot) & 31))
}

// HeightGenerator generates probabilistic tower heights for SkipList nodes
// following a geometric distribution with parameter p = 0.25 and maximum height Lmax = 16.
//
// Concurrency:
// HeightGenerator protects its internal random source with a mutex and is safe
// for concurrent access by multiple goroutines.
type HeightGenerator struct {
	mu  sync.Mutex
	src RandomSource
}

// NewHeightGenerator creates a HeightGenerator backed by the provided RandomSource.
// If src is nil, a default entropy-seeded generator is created.
func NewHeightGenerator(src RandomSource) *HeightGenerator {
	if src == nil {
		return NewDefaultHeightGenerator()
	}
	return &HeightGenerator{
		src: src,
	}
}

// NewDefaultHeightGenerator creates a HeightGenerator backed by a PCG32 generator
// seeded with system entropy.
func NewDefaultHeightGenerator() *HeightGenerator {
	return &HeightGenerator{
		src: NewPCG32(initialEntropySeed(), 1),
	}
}

// initialEntropySeed generates a 64-bit seed from crypto/rand, falling back to UnixNano.
func initialEntropySeed() uint64 {
	var buf [8]byte
	if _, err := cryptorand.Read(buf[:]); err == nil {
		return binary.GetUint64(buf[:])
	}
	return uint64(time.Now().UnixNano())
}

// RandomHeight generates a geometrically distributed height in the range [MinHeight, MaxHeight].
//
// Algorithm:
// Starting at height = 1 (Level 0), each successive level promotion occurs with
// probability p = 0.25 (P(H >= n) = p^(n-1)).
//
// Promotion condition:
// In a uniform 32-bit integer, exactly 1/4 of all values have the two least significant
// bits equal to 00 (val & 3 == 0).
//
// Termination:
// Because the loop checks height < MaxHeight (16), the loop terminates deterministically
// in at most (MaxHeight - MinHeight) = 15 iterations, regardless of random source behavior.
//
// Invariant Guarantees:
//   - P03-S01-INV-01: 1 <= height <= MaxHeight (16).
//   - P03-S01-INV-04: Height depends strictly on the random source, completely independent
//     of user keys and values.
//   - P03-S01-INV-05: Always terminates deterministically.
func (g *HeightGenerator) RandomHeight() int {
	g.mu.Lock()
	defer g.mu.Unlock()

	height := MinHeight
	for height < MaxHeight && (g.src.Uint32()&3 == 0) {
		height++
	}
	return height
}

// package-level default generator for roadmap function randomHeight()
var defaultHeightGen = NewDefaultHeightGenerator()

// randomHeight generates a random height using the package-default HeightGenerator.
func randomHeight() int {
	return defaultHeightGen.RandomHeight()
}
