package wal_test

import (
	stdErrors "errors"
	"fmt"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/wal"
)

// TestSEC_P07_013_InitialSegmentContinuityMatrix validates the complete initial segment ID matrix (P07-SEC-013).
func TestSEC_P07_013_InitialSegmentContinuityMatrix(t *testing.T) {
	tests := []struct {
		name        string
		ids         []uint64
		expectErr   bool
		expectedGap *errors.SegmentGapError
	}{
		{
			name:      "empty slice",
			ids:       []uint64{},
			expectErr: false,
		},
		{
			name:      "single initial segment [1]",
			ids:       []uint64{1},
			expectErr: false,
		},
		{
			name:      "contiguous pairs [1, 2]",
			ids:       []uint64{1, 2},
			expectErr: false,
		},
		{
			name:      "contiguous triple [1, 2, 3]",
			ids:       []uint64{1, 2, 3},
			expectErr: false,
		},
		{
			name:      "single non-initial segment [2]",
			ids:       []uint64{2},
			expectErr: true,
			expectedGap: &errors.SegmentGapError{
				Expected: 1,
				Actual:   2,
			},
		},
		{
			name:      "non-initial pair [3, 4]",
			ids:       []uint64{3, 4},
			expectErr: true,
			expectedGap: &errors.SegmentGapError{
				Expected: 1,
				Actual:   3,
			},
		},
		{
			name:      "non-initial sequence [5, 6, 7]",
			ids:       []uint64{5, 6, 7},
			expectErr: true,
			expectedGap: &errors.SegmentGapError{
				Expected: 1,
				Actual:   5,
			},
		},
		{
			name:      "initial segment present but interior gap [1, 3]",
			ids:       []uint64{1, 3},
			expectErr: true,
			expectedGap: &errors.SegmentGapError{
				Expected: 2,
				Actual:   3,
			},
		},
		{
			name:      "initial segment present but later gap [1, 2, 4]",
			ids:       []uint64{1, 2, 4},
			expectErr: true,
			expectedGap: &errors.SegmentGapError{
				Expected: 3,
				Actual:   4,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := wal.ValidateSegmentContinuity(tc.ids)
			if !tc.expectErr {
				if err != nil {
					t.Fatalf("expected nil error for %v, got %v", tc.ids, err)
				}
				return
			}

			if err == nil {
				t.Fatalf("SECURITY VIOLATION: expected error for %v, got nil", tc.ids)
			}

			if !stdErrors.Is(err, errors.ErrSegmentGap) {
				t.Errorf("expected ErrSegmentGap sentinel match, got %v", err)
			}

			var gapErr *errors.SegmentGapError
			if !stdErrors.As(err, &gapErr) {
				t.Fatalf("expected *errors.SegmentGapError, got %T (%v)", err, err)
			}

			if tc.expectedGap != nil {
				if gapErr.Expected != tc.expectedGap.Expected || gapErr.Actual != tc.expectedGap.Actual {
					t.Errorf("gap mismatch for %v: expected %+v, got %+v", tc.ids, tc.expectedGap, gapErr)
				}
			}
		})
	}
}

// TestSEC_P07_013_EndToEndRecoverWALRejectsNonInitialSegment verifies that RecoverWAL fails closed
// when the lowest WAL segment on disk is not segment 1.
func TestSEC_P07_013_EndToEndRecoverWALRejectsNonInitialSegment(t *testing.T) {
	dbPath := t.TempDir()

	// Write segments 5, 6, 7 (missing segments 1..4)
	if _, err := wal.InitDir(dbPath); err != nil {
		t.Fatalf("InitDir failed: %v", err)
	}

	for segID := uint64(5); segID <= 7; segID++ {
		w, err := wal.CreateSegmentWriter(dbPath, segID)
		if err != nil {
			t.Fatalf("CreateSegmentWriter %d failed: %v", segID, err)
		}
		rec := wal.Record{
			Type:      wal.RecordTypePut,
			SeqNum:    binary.SeqNum(segID),
			Timestamp: 1000,
			Key:       []byte(fmt.Sprintf("k%d", segID)),
			Value:     []byte(fmt.Sprintf("v%d", segID)),
		}
		if err := w.AppendSync(rec); err != nil {
			_ = w.Close()
			t.Fatalf("AppendSync failed: %v", err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("Close segment %d failed: %v", segID, err)
		}
	}

	sink := wal.ReplayFunc(func(rec wal.Record) error {
		t.Fatalf("sink should not be called when segment validation fails")
		return nil
	})

	report, err := wal.RecoverWAL(dbPath, sink)
	if err == nil {
		t.Fatalf("SECURITY VIOLATION: RecoverWAL succeeded on segments [5, 6, 7]")
	}

	if !stdErrors.Is(err, errors.ErrSegmentGap) {
		t.Errorf("expected ErrSegmentGap, got %v", err)
	}

	var gapErr *errors.SegmentGapError
	if !stdErrors.As(err, &gapErr) {
		t.Fatalf("expected *errors.SegmentGapError, got %T (%v)", err, err)
	}
	if gapErr.Expected != 1 || gapErr.Actual != 5 {
		t.Errorf("expected gap (1, 5), got (%d, %d)", gapErr.Expected, gapErr.Actual)
	}

	if report.SegmentCount != 0 {
		t.Errorf("expected 0 segments in report on failure, got %d", report.SegmentCount)
	}
}

// TestSEC_P07_013_EmptyWALRemainsCleanSuccess verifies that empty WAL directory remains valid (clean report, nil error).
func TestSEC_P07_013_EmptyWALRemainsCleanSuccess(t *testing.T) {
	dbPath := t.TempDir()
	if _, err := wal.InitDir(dbPath); err != nil {
		t.Fatalf("InitDir failed: %v", err)
	}

	sinkCalled := false
	sink := wal.ReplayFunc(func(rec wal.Record) error {
		sinkCalled = true
		return nil
	})

	report, err := wal.RecoverWAL(dbPath, sink)
	if err != nil {
		t.Fatalf("expected clean success on empty WAL, got %v", err)
	}
	if sinkCalled {
		t.Errorf("sink should not be called on empty WAL")
	}
	if report.SegmentCount != 0 || report.HighestSegmentID != 0 {
		t.Errorf("expected empty report, got %+v", report)
	}
}
