package metrics

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDiskClassification_BoundaryMatrix(t *testing.T) {
	const (
		gib = 1024 * 1024 * 1024
		mib = 1024 * 1024
	)

	// Helper to create total and free bytes for a specific percentage and byte count
	// If total = 100 GiB, 15% is 15 GiB, 5% is 5 GiB (both well above 2 GiB / 512 MiB).
	// To isolate percentage vs byte limits:
	// Large disk: total = 100 GiB (so byte limits > 2 GiB are easy)
	totalLarge := uint64(100 * gib)

	tests := []struct {
		name       string
		total      uint64
		free       uint64
		wantStatus DiskStatus
	}{
		// --- 15% Boundary (Large disk: free bytes >> 2 GiB) ---
		{
			name:       "15% + epsilon (15.01% with 15.01 GiB)",
			total:      totalLarge,
			free:       uint64(float64(totalLarge) * 0.1501),
			wantStatus: DiskStatusHealthy,
		},
		{
			name:       "15% exact (15.00% with 15.00 GiB)",
			total:      totalLarge,
			free:       uint64(float64(totalLarge) * 0.1500),
			wantStatus: DiskStatusWarning,
		},
		{
			name:       "15% - epsilon (14.99% with 14.99 GiB)",
			total:      totalLarge,
			free:       uint64(float64(totalLarge) * 0.1499),
			wantStatus: DiskStatusWarning,
		},

		// --- 5% Boundary (Large disk: free bytes >> 512 MiB) ---
		{
			name:       "5% + epsilon (5.01% with 5.01 GiB)",
			total:      totalLarge,
			free:       uint64(float64(totalLarge) * 0.0501),
			wantStatus: DiskStatusWarning, // Above 5% but below 15%
		},
		{
			name:       "5% exact (5.00% with 5.00 GiB)",
			total:      totalLarge,
			free:       uint64(float64(totalLarge) * 0.0500),
			wantStatus: DiskStatusCritical,
		},
		{
			name:       "5% - epsilon (4.99% with 4.99 GiB)",
			total:      totalLarge,
			free:       uint64(float64(totalLarge) * 0.0499),
			wantStatus: DiskStatusCritical,
		},

		// --- 2 GiB Boundary (Percentage held at 50% on small disk) ---
		{
			name:       "2 GiB + 1 byte (percentage = 50%)",
			total:      (2*gib + 1) * 2,
			free:       2*gib + 1,
			wantStatus: DiskStatusHealthy,
		},
		{
			name:       "2 GiB exact (percentage = 50%)",
			total:      (2 * gib) * 2,
			free:       2 * gib,
			wantStatus: DiskStatusWarning,
		},
		{
			name:       "2 GiB - 1 byte (percentage = 50%)",
			total:      (2*gib - 1) * 2,
			free:       2*gib - 1,
			wantStatus: DiskStatusWarning,
		},

		// --- 512 MiB Boundary (Percentage held at 50% on very small disk) ---
		{
			name:       "512 MiB + 1 byte (percentage = 50%)",
			total:      (512*mib + 1) * 2,
			free:       512*mib + 1,
			wantStatus: DiskStatusWarning, // <= 2 GiB but > 512 MiB
		},
		{
			name:       "512 MiB exact (percentage = 50%)",
			total:      (512 * mib) * 2,
			free:       512 * mib,
			wantStatus: DiskStatusCritical,
		},
		{
			name:       "512 MiB - 1 byte (percentage = 50%)",
			total:      (512*mib - 1) * 2,
			free:       512*mib - 1,
			wantStatus: DiskStatusCritical,
		},

		// --- Mixed Conditions (Section 17) ---
		// 1. percentage healthy (>15%), bytes warning (<=2 GiB, >512 MiB)
		{
			name:       "percentage healthy (25%), bytes warning (1 GiB)",
			total:      4 * gib,
			free:       1 * gib,
			wantStatus: DiskStatusWarning,
		},
		// 2. percentage warning (<=15%, >5%), bytes healthy (>2 GiB)
		{
			name:       "percentage warning (10%), bytes healthy (10 GiB)",
			total:      100 * gib,
			free:       10 * gib,
			wantStatus: DiskStatusWarning,
		},
		// 3. percentage healthy (>15%), bytes critical (<=512 MiB)
		{
			name:       "percentage healthy (50%), bytes critical (256 MiB)",
			total:      512 * mib,
			free:       256 * mib,
			wantStatus: DiskStatusCritical,
		},
		// 4. percentage critical (<=5%), bytes healthy (>2 GiB)
		{
			name:       "percentage critical (4%), bytes healthy (4 GiB)",
			total:      100 * gib,
			free:       4 * gib,
			wantStatus: DiskStatusCritical,
		},
		// 5. percentage warning (<=15%, >5%), bytes critical (<=512 MiB)
		{
			name:       "percentage warning (10%), bytes critical (400 MiB)",
			total:      4000 * mib,
			free:       400 * mib,
			wantStatus: DiskStatusCritical,
		},
		// 6. percentage critical (<=5%), bytes warning (<=2 GiB, >512 MiB)
		{
			name:       "percentage critical (4%), bytes warning (1.6 GiB)",
			total:      40 * gib,
			free:       1600 * mib,
			wantStatus: DiskStatusCritical,
		},

		// --- Edge / Zero Values ---
		{
			name:       "zero total and zero free",
			total:      0,
			free:       0,
			wantStatus: DiskStatusCritical,
		},
		{
			name:       "zero free on non-zero total",
			total:      10 * gib,
			free:       0,
			wantStatus: DiskStatusCritical,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sample := ClassifyDisk(tc.total, tc.free, nil)
			if sample.Status != tc.wantStatus {
				t.Errorf("ClassifyDisk(%d, %d) status = %v (%.2f%%, %d bytes); want %v",
					tc.total, tc.free, sample.Status, sample.FreePercent, sample.FreeBytes, tc.wantStatus)
			}
		})
	}
}

func TestDiskClassification_StatfsError(t *testing.T) {
	statErr := errors.New("i/o error reading superblock")
	sample := ClassifyDisk(0, 0, statErr)
	if sample.Status != DiskStatusUnknown {
		t.Fatalf("expected status %v on statfs error, got %v", DiskStatusUnknown, sample.Status)
	}
	if sample.Err != statErr {
		t.Fatalf("expected error %v, got %v", statErr, sample.Err)
	}
}

func TestDiskSampler_CacheTTLAndSingleflight(t *testing.T) {
	var statfsCalls atomic.Int64

	mockTotal := uint64(50 * 1024 * 1024 * 1024)
	mockFree := uint64(20 * 1024 * 1024 * 1024)

	sampler := NewDiskSampler("/dummy/path", 100*time.Millisecond)
	sampler.SetStatfsForTesting(func(path string) (uint64, uint64, error) {
		statfsCalls.Add(1)
		time.Sleep(10 * time.Millisecond) // Simulate slow filesystem syscall
		return mockTotal, mockFree, nil
	})

	// Initial sample
	s1 := sampler.Sample()
	if s1.Status != DiskStatusHealthy {
		t.Fatalf("expected healthy status, got %v", s1.Status)
	}
	if calls := statfsCalls.Load(); calls != 1 {
		t.Fatalf("expected exactly 1 statfs call, got %d", calls)
	}

	// 50 concurrent calls within TTL window (should all read cached sample, 0 extra calls)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := sampler.Sample()
			if s.Status != DiskStatusHealthy {
				t.Errorf("expected healthy status in concurrent scrape, got %v", s.Status)
			}
		}()
	}
	wg.Wait()

	if calls := statfsCalls.Load(); calls != 1 {
		t.Fatalf("expected exactly 1 statfs call within TTL window, got %d", calls)
	}

	// Wait for TTL to expire
	time.Sleep(120 * time.Millisecond)

	// 50 concurrent calls after expiration (stampede protection: exactly 1 call should refresh)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := sampler.Sample()
			if s.Status != DiskStatusHealthy {
				t.Errorf("expected healthy status after refresh, got %v", s.Status)
			}
		}()
	}
	wg.Wait()

	if calls := statfsCalls.Load(); calls != 2 {
		t.Fatalf("expected exactly 2 total statfs calls after singleflight refresh, got %d", calls)
	}
}

func TestDiskSampler_NilSafety(t *testing.T) {
	var s *DiskSampler
	sample := s.Sample()
	if sample.Status != DiskStatusUnknown {
		t.Fatalf("expected nil sampler to return DiskStatusUnknown, got %v", sample.Status)
	}
	if sample.Err == nil {
		t.Fatalf("expected non-nil error from nil sampler")
	}
}

func TestDiskSampler_LiveSyscall(t *testing.T) {
	// Query current working directory on host OS
	sampler := NewDiskSampler(".", DefaultDiskSampleTTL)
	sample := sampler.Sample()

	if sample.Status == DiskStatusUnknown {
		t.Fatalf("live statfs returned Unknown status: %v", sample.Err)
	}
	if sample.TotalBytes == 0 {
		t.Fatalf("live statfs reported 0 total bytes")
	}
	if sample.SampledAt.IsZero() {
		t.Fatalf("expected non-zero SampledAt timestamp")
	}
}
