package wal

import (
	stdErrors "errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
)

// ReplaySink consumes valid Write-Ahead Log records during multi-segment crash recovery.
//
// Implementations receive each record sequentially in strictly ascending physical log order.
// ReplaySink is strictly an engine boundary interface; implementations (such as the Phase 03
// MemTable) are decoupled from the WAL recovery coordinator.
type ReplaySink interface {
	Apply(rec Record) error
}

// ReplayFunc is an adapter allowing the use of ordinary functions as a ReplaySink.
type ReplayFunc func(rec Record) error

// Apply calls f(rec).
func (f ReplayFunc) Apply(rec Record) error {
	return f(rec)
}

// RecoveryReport details the diagnostic outcome of a multi-segment WAL recovery operation.
type RecoveryReport struct {
	// SegmentCount is the total number of valid segment files discovered and processed.
	SegmentCount int

	// HighestSegmentID is the numeric ID of the highest (latest) segment processed.
	// 0 if the WAL directory contains no segment files.
	HighestSegmentID uint64

	// ValidRecords is the total number of complete, checksum-verified records recovered across all segments.
	ValidRecords int

	// ReplayedRecords is the number of valid records successfully applied to the ReplaySink.
	// In a fully successful recovery, ReplayedRecords == ValidRecords.
	// If the sink returns an error mid-replay, ReplayedRecords reflects the count applied prior to the error.
	ReplayedRecords int

	// Truncated reports whether the latest segment underwent physical torn-tail truncation.
	// Historical segments are never truncated.
	Truncated bool

	// TruncatedBytes is the number of torn tail bytes removed from the latest segment during recovery.
	// 0 if no truncation occurred.
	TruncatedBytes int64

	// LastSeqNum is the highest sequence number observed among all replayed records.
	// 0 if no records exist in the WAL.
	LastSeqNum binary.SeqNum
}

// RecoverWAL coordinates multi-segment crash recovery across a database WAL directory.
//
// Operational Contract & Recovery Invariants:
//  1. Discovery & Ordering:
//     Discovers all segment files under <db_path>/wal/ using ListSegments and sorts them
//     strictly in ascending numeric segment ID order (e.g. 1 -> 2 -> 3 -> 10).
//  2. No Segment ID Gaps:
//     Discovered segment IDs must form a contiguous sequence (ids[i] == ids[i-1] + 1).
//     If a gap is detected (e.g. 1, 2, 4), recovery fails immediately with *errors.SegmentGapError.
//  3. No Duplicate Segment IDs:
//     Duplicate numeric IDs fail recovery immediately with *errors.DuplicateSegmentError.
//  4. Clean Empty WAL Directory:
//     If the WAL directory contains zero segment files, returns an explicit clean RecoveryReport{}
//     with nil error without creating files.
//  5. Historical Segment Inviolability:
//     Historical sealed segments (1..N-1) MUST be completely clean, ending at clean io.EOF.
//     If any historical segment contains a torn tail, bit-rot corruption, or invalid record type,
//     recovery FAILS CLOSED immediately without modifying any file on disk. Historical segments
//     are never truncated.
//  6. Latest Segment Tail Recovery:
//     The latest segment (N) may legally contain an incomplete torn tail at EOF caused by a crash
//     during the most recent append. If present, RecoverSegment safely truncates the tail to the
//     last valid record boundary. Complete CRC corruption or middle corruption in the latest
//     segment fails closed without truncation.
//  7. Deterministic Streaming Replay & Sequence Monotonicity:
//     Valid records are streamed in ascending segment order and evaluated sequentially.
//     Sequence numbers must be strictly monotonically increasing (rec.SeqNum > prev.SeqNum).
//     If a duplicate or decreasing sequence number is encountered, recovery fails with
//     *errors.SequenceOutOfOrderError.
//  8. Replay Sink Integration:
//     Each valid record is passed to sink.Apply(rec). If sink is nil, records are validated
//     and counted without sink invocation (validation-only mode). If sink.Apply returns an
//     error, replay stops immediately and propagates the error; report.ReplayedRecords
//     reflects the count applied prior to the error.
//  9. Two-Phase Ordering & Mutation Transparency:
//     Physical repair of the latest segment occurs before logical replay. If truncation
//     succeeds on disk and sink replay subsequently fails, report.Truncated remains true,
//     honestly reflecting the physical filesystem state.
//  10. Quiescent Startup Assumption:
//     Assumes WAL segments are quiescent (no concurrent RotatingWriter is running).
func RecoverWAL(dbPath string, sink ReplaySink) (RecoveryReport, error) {
	if dbPath == "" {
		return RecoveryReport{}, fmt.Errorf("%w: db path cannot be empty", os.ErrInvalid)
	}

	cleanDBPath := filepath.Clean(dbPath)

	// Step 1: Discover segment IDs in strictly ascending numeric order
	ids, err := ListSegments(cleanDBPath)
	if err != nil {
		return RecoveryReport{}, fmt.Errorf("wal: failed to discover segments: %w", err)
	}

	// Step 2: Handle empty WAL directory
	if len(ids) == 0 {
		return RecoveryReport{}, nil
	}

	// Step 3: Validate segment ID continuity (no duplicates, no gaps)
	if err := ValidateSegmentContinuity(ids); err != nil {
		return RecoveryReport{}, err
	}

	report := RecoveryReport{
		SegmentCount:     len(ids),
		HighestSegmentID: ids[len(ids)-1],
	}

	// Step 4: Phase 1 - Verify all historical sealed segments (1..N-1) in read-only mode.
	// Historical segments MUST be completely valid and end at clean io.EOF.
	// Any torn tail, corruption, or sequence regression in a historical segment halts recovery
	// immediately without mutating any file on disk.
	var (
		hasHistSeq  bool
		prevHistSeq binary.SeqNum
	)

	for i := 0; i < len(ids)-1; i++ {
		histID := ids[i]
		reader, err := OpenSegmentReader(cleanDBPath, histID)
		if err != nil {
			return report, fmt.Errorf("wal: failed to open historical segment %d: %w", histID, err)
		}

		for {
			rec, nextErr := reader.Next()
			if nextErr == nil {
				if !hasHistSeq {
					hasHistSeq = true
					prevHistSeq = rec.SeqNum
				} else {
					if rec.SeqNum <= prevHistSeq {
						_ = reader.Close()
						return report, &errors.SequenceOutOfOrderError{
							Previous: uint64(prevHistSeq),
							Current:  uint64(rec.SeqNum),
						}
					}
					prevHistSeq = rec.SeqNum
				}
				continue
			}
			if stdErrors.Is(nextErr, io.EOF) {
				break
			}
			_ = reader.Close()
			if stdErrors.Is(nextErr, errors.ErrHeaderTruncated) || stdErrors.Is(nextErr, io.ErrUnexpectedEOF) {
				return report, fmt.Errorf("wal: historical segment %d contains torn tail: %w", histID, nextErr)
			}
			return report, fmt.Errorf("wal: historical segment %d is corrupted: %w", histID, nextErr)
		}

		if closeErr := reader.Close(); closeErr != nil {
			return report, fmt.Errorf("wal: failed to close historical segment %d: %w", histID, closeErr)
		}
	}

	// Step 5: Phase 2 - Inspect and recover the latest segment (N).
	// Only the latest segment may legitimately contain a torn tail at EOF.
	latestID := ids[len(ids)-1]
	latestPath := SegmentPath(cleanDBPath, latestID)

	statInfo, statErr := os.Lstat(latestPath)
	if statErr != nil {
		return report, fmt.Errorf("wal: failed to stat latest segment %d: %w", latestID, statErr)
	}
	initialLatestSize := statInfo.Size()

	recRes, recErr := RecoverSegment(latestPath)
	if recErr != nil {
		report.Truncated = recRes.Truncated
		return report, fmt.Errorf("wal: failed to recover latest segment %d: %w", latestID, recErr)
	}

	if recRes.Truncated {
		report.Truncated = true
		report.TruncatedBytes = initialLatestSize - recRes.RecoveredOffset
	}

	// Step 6: Phase 3 - Logical Streaming Replay & Global Sequence Monotonicity.
	// All segments on disk are now verified and physically clean.
	var (
		hasPrevSeq bool
		prevSeqNum binary.SeqNum
	)

	for _, segID := range ids {
		reader, err := OpenSegmentReader(cleanDBPath, segID)
		if err != nil {
			return report, fmt.Errorf("wal: failed to open segment %d for replay: %w", segID, err)
		}

		for {
			rec, nextErr := reader.Next()
			if nextErr != nil {
				if stdErrors.Is(nextErr, io.EOF) {
					break
				}
				_ = reader.Close()
				return report, fmt.Errorf("wal: read error in segment %d at offset %d: %w", segID, reader.Offset(), nextErr)
			}

			// Validate global sequence monotonicity: rec.SeqNum > prevSeqNum
			if !hasPrevSeq {
				hasPrevSeq = true
				prevSeqNum = rec.SeqNum
			} else {
				if rec.SeqNum <= prevSeqNum {
					_ = reader.Close()
					return report, &errors.SequenceOutOfOrderError{
						Previous: uint64(prevSeqNum),
						Current:  uint64(rec.SeqNum),
					}
				}
				prevSeqNum = rec.SeqNum
			}

			report.ValidRecords++
			report.LastSeqNum = rec.SeqNum

			if sink != nil {
				if applyErr := sink.Apply(rec); applyErr != nil {
					_ = reader.Close()
					return report, fmt.Errorf("wal: replay sink failed on record %d (seq %s): %w", report.ReplayedRecords+1, rec.SeqNum, applyErr)
				}
				report.ReplayedRecords++
			} else {
				report.ReplayedRecords++
			}
		}

		if closeErr := reader.Close(); closeErr != nil {
			return report, fmt.Errorf("wal: failed to close segment %d after replay: %w", segID, closeErr)
		}
	}

	return report, nil
}

// ValidateSegmentContinuity validates that a sorted slice of segment IDs has no duplicates and no gaps.
func ValidateSegmentContinuity(ids []uint64) error {
	for i := 1; i < len(ids); i++ {
		if ids[i] == ids[i-1] {
			return &errors.DuplicateSegmentError{SegmentID: ids[i]}
		}
		if ids[i] != ids[i-1]+1 {
			return &errors.SegmentGapError{
				Expected: ids[i-1] + 1,
				Actual:   ids[i],
			}
		}
	}
	return nil
}
