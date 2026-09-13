package engine

import (
	"context"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/silent-knight19/lattice/internal/errors"
)

// Default backpressure thresholds and limits (SEC-003)
const (
	DefaultMaxMemoryBytes = 256 * 1024 * 1024 // 256 MiB
	DefaultHighWatermark  = 0.80              // 80% capacity -> begin proportional write throttling
	DefaultHardWatermark  = 0.95              // 95% capacity -> pause / reject incoming writes
	DefaultMaxWaitTimeout = 5 * time.Second   // Maximum time a writer may wait before rejection
)

// BackpressureConfig configures the memory backpressure controller.
type BackpressureConfig struct {
	MaxMemoryBytes uint64
	HighWatermark  float64
	HardWatermark  float64
	MaxWaitTimeout time.Duration
}

// DefaultBackpressureConfig returns default production thresholds.
func DefaultBackpressureConfig() BackpressureConfig {
	return BackpressureConfig{
		MaxMemoryBytes: DefaultMaxMemoryBytes,
		HighWatermark:  DefaultHighWatermark,
		HardWatermark:  DefaultHardWatermark,
		MaxWaitTimeout: DefaultMaxWaitTimeout,
	}
}

// BackpressureStats captures operational telemetry for backpressure events.
type BackpressureStats struct {
	CurrentMemoryBytes uint64  `json:"current_memory_bytes"`
	MaxMemoryBytes     uint64  `json:"max_memory_bytes"`
	Utilization        float64 `json:"utilization"`
	ThrottledWrites    uint64  `json:"throttled_writes"`
	RejectedWrites     uint64  `json:"rejected_writes"`
	ActiveWriters      int64   `json:"active_writers"`
}

// BackpressureController enforces memory growth limits on incoming writes,
// preventing resource exhaustion (CWE-400, SEC-003).
//
// Operational Invariants:
//  1. When memory utilization < HighWatermark, writes proceed with zero delay.
//  2. When HighWatermark <= utilization < HardWatermark, writes experience progressive
//     proportional backpressure delay (up to 50ms) to allow background flushes to catch up.
//  3. When utilization >= HardWatermark, writers are paused on a condition variable until
//     memory is released or context/timeout expires. If capacity remains exhausted, the
//     write is rejected fail-closed with errors.ErrMemoryLimitExceeded.
type BackpressureController struct {
	cfg            BackpressureConfig
	mu             sync.Mutex
	cond           *sync.Cond
	currentBytes   atomic.Uint64
	activeWriters  atomic.Int64
	throttledCount atomic.Uint64
	rejectedCount  atomic.Uint64
	closed         atomic.Bool
}

// NewBackpressureController initializes a BackpressureController with validated configuration.
func NewBackpressureController(cfg BackpressureConfig) *BackpressureController {
	if cfg.MaxMemoryBytes == 0 {
		cfg.MaxMemoryBytes = DefaultMaxMemoryBytes
	}
	if cfg.HighWatermark <= 0 || cfg.HighWatermark >= 1.0 {
		cfg.HighWatermark = DefaultHighWatermark
	}
	if cfg.HardWatermark <= cfg.HighWatermark || cfg.HardWatermark >= 1.0 {
		cfg.HardWatermark = DefaultHardWatermark
	}
	if cfg.MaxWaitTimeout <= 0 {
		cfg.MaxWaitTimeout = DefaultMaxWaitTimeout
	}

	bc := &BackpressureController{
		cfg: cfg,
	}
	bc.cond = sync.NewCond(&bc.mu)
	return bc
}

// Acquire requests permission to allocate bytesNeeded for an incoming write.
// If memory limits are exceeded, Acquire blocks until space is released or returns
// ErrMemoryLimitExceeded upon timeout/context cancellation.
func (bc *BackpressureController) Acquire(ctx context.Context, bytesNeeded uint64) error {
	if bc == nil {
		return nil
	}
	if bc.closed.Load() {
		return errors.ErrWriterClosed
	}

	hardCeiling := uint64(float64(bc.cfg.MaxMemoryBytes) * bc.cfg.HardWatermark)
	highWatermark := uint64(float64(bc.cfg.MaxMemoryBytes) * bc.cfg.HighWatermark)

	// Quick check: if single write exceeds total ceiling, reject immediately
	if bytesNeeded > hardCeiling {
		bc.rejectedCount.Add(1)
		return errors.ErrMemoryLimitExceeded
	}

	bc.activeWriters.Add(1)
	defer bc.activeWriters.Add(-1)

	startTime := time.Now()
	timeout := bc.cfg.MaxWaitTimeout

	bc.mu.Lock()
	defer bc.mu.Unlock()

	for {
		if bc.closed.Load() {
			return errors.ErrWriterClosed
		}

		cur := bc.currentBytes.Load()
		if cur <= math.MaxUint64-bytesNeeded && cur+bytesNeeded <= hardCeiling {
			// Space is available below hard ceiling
			bc.currentBytes.Add(bytesNeeded)

			// Check if we need progressive throttling (high watermark zone)
			if cur+bytesNeeded >= highWatermark {
				bc.throttledCount.Add(1)
				// Unlock during throttle sleep to prevent serializing all threads
				bc.mu.Unlock()
				ratio := float64(cur+bytesNeeded-highWatermark) / float64(hardCeiling-highWatermark)
				delay := time.Duration(ratio * float64(50*time.Millisecond))
				if delay > 0 {
					select {
					case <-time.After(delay):
					case <-ctx.Done():
						// Release acquired memory on aborted delay
						bc.Release(bytesNeeded)
						return ctx.Err()
					}
				}
				bc.mu.Lock()
			}
			return nil
		}

		// Hard limit exceeded: check timeout and context
		if time.Since(startTime) >= timeout {
			bc.rejectedCount.Add(1)
			return errors.ErrMemoryLimitExceeded
		}

		if ctx.Err() != nil {
			return ctx.Err()
		}

		// Non-blocking wait using Wait with timeout goroutine or Broadcast signaling
		waitChan := make(chan struct{})
		go func() {
			bc.mu.Lock()
			defer bc.mu.Unlock()
			select {
			case <-waitChan:
				return
			default:
				bc.cond.Wait()
				close(waitChan)
			}
		}()

		bc.mu.Unlock()
		select {
		case <-waitChan:
			bc.mu.Lock()
		case <-time.After(timeout - time.Since(startTime)):
			bc.mu.Lock()
			bc.cond.Broadcast()
			bc.rejectedCount.Add(1)
			return errors.ErrMemoryLimitExceeded
		case <-ctx.Done():
			bc.mu.Lock()
			bc.cond.Broadcast()
			return ctx.Err()
		}
	}
}

// Release returns allocated memory bytes, notifying waiting writers.
func (bc *BackpressureController) Release(bytesFreed uint64) {
	if bc == nil || bytesFreed == 0 {
		return
	}
	for {
		cur := bc.currentBytes.Load()
		if bytesFreed >= cur {
			if bc.currentBytes.CompareAndSwap(cur, 0) {
				break
			}
		} else {
			if bc.currentBytes.CompareAndSwap(cur, cur-bytesFreed) {
				break
			}
		}
	}

	bc.mu.Lock()
	bc.cond.Broadcast()
	bc.mu.Unlock()
}

// RecordUsage directly synchronizes the tracked memory usage (e.g. from SkipList.ByteSize()).
func (bc *BackpressureController) RecordUsage(totalBytes uint64) {
	if bc == nil {
		return
	}
	prev := bc.currentBytes.Swap(totalBytes)
	if totalBytes < prev {
		bc.mu.Lock()
		bc.cond.Broadcast()
		bc.mu.Unlock()
	}
}

// CurrentBytes returns the current memory usage tracked by the controller.
func (bc *BackpressureController) CurrentBytes() uint64 {
	if bc == nil {
		return 0
	}
	return bc.currentBytes.Load()
}

// IsThrottled reports whether current memory utilization has crossed HighWatermark.
func (bc *BackpressureController) IsThrottled() bool {
	if bc == nil {
		return false
	}
	high := uint64(float64(bc.cfg.MaxMemoryBytes) * bc.cfg.HighWatermark)
	return bc.currentBytes.Load() >= high
}

// IsOverCapacity reports whether memory utilization has crossed HardWatermark.
func (bc *BackpressureController) IsOverCapacity() bool {
	if bc == nil {
		return false
	}
	hard := uint64(float64(bc.cfg.MaxMemoryBytes) * bc.cfg.HardWatermark)
	return bc.currentBytes.Load() >= hard
}

// Stats returns a snapshot of backpressure telemetry.
func (bc *BackpressureController) Stats() BackpressureStats {
	if bc == nil {
		return BackpressureStats{}
	}
	cur := bc.currentBytes.Load()
	util := 0.0
	if bc.cfg.MaxMemoryBytes > 0 {
		util = float64(cur) / float64(bc.cfg.MaxMemoryBytes)
	}
	return BackpressureStats{
		CurrentMemoryBytes: cur,
		MaxMemoryBytes:     bc.cfg.MaxMemoryBytes,
		Utilization:        util,
		ThrottledWrites:    bc.throttledCount.Load(),
		RejectedWrites:     bc.rejectedCount.Load(),
		ActiveWriters:      bc.activeWriters.Load(),
	}
}

// Close permanently shuts down the controller and releases all waiting writers.
func (bc *BackpressureController) Close() {
	if bc == nil {
		return
	}
	bc.closed.Store(true)
	bc.mu.Lock()
	bc.cond.Broadcast()
	bc.mu.Unlock()
}
