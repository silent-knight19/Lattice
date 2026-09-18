package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand"
	"sync"
	"time"

	"github.com/silent-knight19/lattice/internal/benchmark"
	"github.com/silent-knight19/lattice/internal/transport"
)

// worker maintains isolated benchmark state and a dedicated persistent connection.
type worker struct {
	id        int
	cfg       *Config
	client    *BenchClient
	rng       *rand.Rand
	zipf      *benchmark.ZipfGenerator
	getHist   *benchmark.LatencyHistogram
	putHist   *benchmark.LatencyHistogram
	keyBuf    [64]byte
	valBuf    []byte
	gets      uint64
	puts      uint64
	getErrors uint64
	putErrors uint64
	netErrors uint64
}

// Run coordinates the benchmark execution: optional pre-population, worker connection
// establishment, synchronized duration loop, and post-run histogram aggregation.
func Run(ctx context.Context, cfg *Config, stdout, stderr io.Writer) (*Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if cfg == nil {
		return nil, fmt.Errorf("config cannot be nil")
	}

	result := &Result{
		Config:  *cfg,
		GetHist: benchmark.NewLatencyHistogram(),
		PutHist: benchmark.NewLatencyHistogram(),
	}

	// Step 1: Pre-population Phase (if enabled and applicable)
	if cfg.Populate && cfg.Workload != WorkloadWrite && cfg.PopulateKeys > 0 {
		popStart := time.Now()
		if stdout != nil {
			fmt.Fprintf(stdout, "lattice-bench: pre-populating %s keys into %s...\n",
				formatUint(cfg.PopulateKeys), cfg.Address)
		}
		if err := runPrepopulation(ctx, cfg); err != nil {
			return nil, fmt.Errorf("pre-population failed: %w", err)
		}
		result.PopulateElapsed = time.Since(popStart)
		result.PopulatedCount = cfg.PopulateKeys
		if stdout != nil {
			fmt.Fprintf(stdout, "lattice-bench: pre-population complete in %v\n",
				result.PopulateElapsed.Round(time.Millisecond))
		}
	}

	// Step 2: Establish Worker Connections
	workers := make([]*worker, cfg.Concurrency)
	for i := 0; i < cfg.Concurrency; i++ {
		client, err := DialContext(ctx, cfg.Address, cfg.Timeout)
		if err != nil {
			// Cleanup previously established connections
			for j := 0; j < i; j++ {
				if workers[j] != nil && workers[j].client != nil {
					_ = workers[j].client.Close()
					workers[j].client = nil
				}
			}
			return nil, fmt.Errorf("worker %d failed to connect to %s: %w", i, cfg.Address, err)
		}

		// Isolated PRNG per worker derived deterministically from base seed
		workerSeed := cfg.Seed + int64(i)*10007 + 1
		src := rand.NewSource(workerSeed)
		workerRng := rand.New(src)

		zipfGen, err := benchmark.NewZipfGenerator(cfg.Keyspace, benchmark.DefaultZipfTheta, workerSeed)
		if err != nil {
			_ = client.Close()
			for j := 0; j < i; j++ {
				if workers[j] != nil && workers[j].client != nil {
					_ = workers[j].client.Close()
					workers[j].client = nil
				}
			}
			return nil, fmt.Errorf("worker %d zipf initialization failed: %w", i, err)
		}

		valBuf := make([]byte, cfg.ValSize)
		// Populate value buffer with deterministic repeating ASCII pattern
		if cfg.ValSize > 0 {
			for k := range valBuf {
				valBuf[k] = byte('a' + (k % 26))
			}
		}

		workers[i] = &worker{
			id:      i,
			cfg:     cfg,
			client:  client,
			rng:     workerRng,
			zipf:    zipfGen,
			getHist: benchmark.NewLatencyHistogram(),
			putHist: benchmark.NewLatencyHistogram(),
			valBuf:  valBuf,
		}
	}

	// Step 3: Timed Benchmark Execution
	runCtx, cancel := context.WithTimeout(ctx, cfg.Duration)
	defer cancel()

	if stdout != nil {
		fmt.Fprintf(stdout, "lattice-bench: starting %d workers for duration %v...\n",
			cfg.Concurrency, cfg.Duration)
	}

	var wg sync.WaitGroup
	startTime := time.Now()

	for _, w := range workers {
		wg.Add(1)
		go w.run(runCtx, &wg)
	}

	wg.Wait()
	result.Elapsed = time.Since(startTime)

	// Step 4: Post-Run Aggregation
	for _, w := range workers {
		_ = result.GetHist.Merge(w.getHist)
		_ = result.PutHist.Merge(w.putHist)
		result.GetCount += w.gets
		result.PutCount += w.puts
		result.GetErrors += w.getErrors
		result.PutErrors += w.putErrors
		result.NetErrors += w.netErrors
	}

	result.TotalOps = result.GetCount + result.PutCount
	if result.Elapsed > 0 {
		result.Throughput = float64(result.TotalOps) / result.Elapsed.Seconds()
	}

	return result, nil
}

const (
	minReconnectBackoff = 10 * time.Millisecond
	maxReconnectBackoff = 1 * time.Second
)

// run executes the worker measurement loop until runCtx expires.
func (w *worker) run(runCtx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()
	defer func() {
		if w.client != nil {
			_ = w.client.Close()
			w.client = nil
		}
	}()

	isMixed := w.cfg.Workload == WorkloadMixed
	isPureRead := w.cfg.Workload == WorkloadRead
	isZipfian := w.cfg.KeyDistribution == DistributionZipfian
	readRatio := w.cfg.ReadRatio

	backoff := minReconnectBackoff

	for {
		select {
		case <-runCtx.Done():
			return
		default:
		}

		// Check client availability. If disconnected, attempt reconnection with bounded backoff.
		if w.client == nil {
			if !w.reconnect(runCtx, &backoff) {
				// Reconnection failed or runCtx cancelled; retry in next iteration (which checks runCtx.Done())
				// and NEVER proceed to execute an operation with a nil client.
				continue
			}
			// Successful reconnection: reset backoff interval
			backoff = minReconnectBackoff
		}

		// 1. Operation Selection (hot path)
		isRead := false
		if isPureRead {
			isRead = true
		} else if isMixed {
			if readRatio >= 1.0 {
				isRead = true
			} else if readRatio <= 0.0 {
				isRead = false
			} else {
				isRead = w.rng.Float64() < readRatio
			}
		}

		// 2. Key Generation (hot path)
		var key []byte
		if isZipfian {
			key = w.zipf.NextKeyBuf(w.keyBuf[:0])
		} else {
			rank := w.rng.Uint64() % w.cfg.Keyspace
			key = w.zipf.FormatKey(w.keyBuf[:0], rank)
		}

		// 3. Timed Request/Response Execution
		t0 := time.Now()
		if isRead {
			resp, err := w.client.Get(runCtx, key)
			latency := time.Since(t0)

			if err != nil {
				w.netErrors++
				if w.client != nil {
					_ = w.client.Close()
					w.client = nil
				}
				continue
			}

			if resp.Status == transport.StatusOk {
				w.gets++
				w.getHist.RecordNano(latency.Nanoseconds())
			} else {
				// Server returned an error status (e.g. StatusKeyNotFound or StatusError)
				w.getErrors++
			}
		} else {
			resp, err := w.client.Put(runCtx, key, w.valBuf)
			latency := time.Since(t0)

			if err != nil {
				w.netErrors++
				if w.client != nil {
					_ = w.client.Close()
					w.client = nil
				}
				continue
			}

			if resp.Status == transport.StatusOk {
				w.puts++
				w.putHist.RecordNano(latency.Nanoseconds())
			} else {
				w.putErrors++
			}
		}
	}
}

// reconnect attempts to re-establish a dropped TCP connection within runCtx.
// Returns true on success, or false if dial failed or runCtx was cancelled.
// Applies bounded exponential backoff between reconnect attempts.
func (w *worker) reconnect(runCtx context.Context, backoff *time.Duration) bool {
	if w.client != nil {
		_ = w.client.Close()
		w.client = nil
	}

	select {
	case <-runCtx.Done():
		return false
	case <-time.After(*backoff):
	}

	nextBackoff := *backoff * 2
	if nextBackoff > maxReconnectBackoff {
		nextBackoff = maxReconnectBackoff
	}
	*backoff = nextBackoff

	c, err := DialContext(runCtx, w.cfg.Address, w.cfg.Timeout)
	if err != nil {
		return false
	}

	w.client = c
	return true
}

// runPrepopulation sequentially writes keys [0, count-1] using a dedicated client connection.
func runPrepopulation(ctx context.Context, cfg *Config) error {
	client, err := DialContext(ctx, cfg.Address, cfg.Timeout)
	if err != nil {
		return fmt.Errorf("pre-population dial: %w", err)
	}
	defer client.Close()

	// Pre-allocate value buffer
	valBuf := bytes.Repeat([]byte("p"), cfg.ValSize)

	// Temporary formatter
	zipf, err := benchmark.NewDefaultZipfGenerator(cfg.Keyspace, cfg.Seed)
	if err != nil {
		return fmt.Errorf("pre-population zipf init: %w", err)
	}

	var keyBuf [64]byte
	for i := uint64(0); i < cfg.PopulateKeys; i++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		key := zipf.FormatKey(keyBuf[:0], i)
		resp, err := client.Put(ctx, key, valBuf)
		if err != nil {
			return fmt.Errorf("pre-population put key %d: %w", i, err)
		}
		if resp.Status != transport.StatusOk {
			return fmt.Errorf("pre-population put key %d failed with status %s: %s",
				i, resp.Status, resp.Message)
		}
	}

	return nil
}
