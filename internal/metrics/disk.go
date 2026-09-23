package metrics

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// DiskStatus defines the operational health status of local storage disk.
type DiskStatus string

const (
	DiskStatusHealthy  DiskStatus = "healthy"
	DiskStatusWarning  DiskStatus = "warning"
	DiskStatusCritical DiskStatus = "critical"
	DiskStatusUnknown  DiskStatus = "unknown"
)

const (
	// Healthy boundaries
	DiskHealthyMinFreePercent = 15.0                   // Free space must exceed 15% AND
	DiskHealthyMinFreeBytes   = 2 * 1024 * 1024 * 1024 // 2 GiB

	// Critical boundaries
	DiskCriticalMaxFreePercent = 5.0               // Free space <= 5% OR
	DiskCriticalMaxFreeBytes   = 512 * 1024 * 1024 // 512 MiB

	// DefaultDiskSampleTTL is the default caching window (5 seconds) to prevent filesystem stat stampedes.
	DefaultDiskSampleTTL = 5 * time.Second
)

// DiskSample captures a point-in-time measurement of filesystem capacity.
type DiskSample struct {
	TotalBytes  uint64
	FreeBytes   uint64
	UsedBytes   uint64
	FreePercent float64
	Status      DiskStatus
	SampledAt   time.Time
	Err         error
}

// ClassifyDisk evaluates filesystem capacity numbers and classifies disk status.
// Precedence is strictly: CRITICAL > WARNING > HEALTHY.
func ClassifyDisk(totalBytes, freeBytes uint64, statErr error) DiskSample {
	now := time.Now()
	if statErr != nil {
		return DiskSample{
			TotalBytes: totalBytes,
			FreeBytes:  freeBytes,
			Status:     DiskStatusUnknown,
			SampledAt:  now,
			Err:        statErr,
		}
	}

	var usedBytes uint64
	if totalBytes >= freeBytes {
		usedBytes = totalBytes - freeBytes
	}

	var freePercent float64
	if totalBytes > 0 {
		freePercent = (float64(freeBytes) / float64(totalBytes)) * 100.0
	}

	sample := DiskSample{
		TotalBytes:  totalBytes,
		FreeBytes:   freeBytes,
		UsedBytes:   usedBytes,
		FreePercent: freePercent,
		SampledAt:   now,
	}

	// 1. Critical precedence: free_percent <= 5% OR free_bytes <= 512 MiB
	if totalBytes == 0 || freePercent <= DiskCriticalMaxFreePercent || freeBytes <= DiskCriticalMaxFreeBytes {
		sample.Status = DiskStatusCritical
		return sample
	}

	// 2. Warning precedence: free_percent <= 15% OR free_bytes <= 2 GiB
	if freePercent <= DiskHealthyMinFreePercent || freeBytes <= DiskHealthyMinFreeBytes {
		sample.Status = DiskStatusWarning
		return sample
	}

	// 3. Healthy: free_percent > 15% AND free_bytes > 2 GiB
	sample.Status = DiskStatusHealthy
	return sample
}

// DiskSampler provides cached, bounded filesystem capacity sampling.
type DiskSampler struct {
	path      string
	ttl       time.Duration
	statfsFn  func(path string) (uint64, uint64, error)
	refreshMu sync.Mutex
	sample    atomic.Pointer[DiskSample]
}

// NewDiskSampler constructs a disk sampler monitoring the filesystem containing path.
func NewDiskSampler(path string, ttl time.Duration) *DiskSampler {
	if ttl <= 0 {
		ttl = DefaultDiskSampleTTL
	}
	return &DiskSampler{
		path:     path,
		ttl:      ttl,
		statfsFn: platformStatfs,
	}
}

// SetStatfsForTesting overrides the filesystem query function for deterministic testing.
func (s *DiskSampler) SetStatfsForTesting(fn func(path string) (uint64, uint64, error)) {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	s.statfsFn = fn
	s.sample.Store(nil) // Invalidate cached sample
}

// Sample returns the current filesystem sample, refreshing from the filesystem if
// the cached sample has expired or does not exist. Concurrent calls during an expired
// cache window are synchronized through a singleflight refresh lock.
func (s *DiskSampler) Sample() DiskSample {
	if s == nil {
		return DiskSample{
			Status:    DiskStatusUnknown,
			SampledAt: time.Now(),
			Err:       errors.New("disk sampler is nil"),
		}
	}

	// Lock-free read path if cache is fresh
	cur := s.sample.Load()
	if cur != nil && time.Since(cur.SampledAt) < s.ttl {
		return *cur
	}

	// Synchronized singleflight refresh
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()

	// Double-check if another goroutine completed refresh while we waited
	cur = s.sample.Load()
	if cur != nil && time.Since(cur.SampledAt) < s.ttl {
		return *cur
	}

	total, free, err := s.statfsFn(s.path)
	newSample := ClassifyDisk(total, free, err)
	s.sample.Store(&newSample)
	return newSample
}
