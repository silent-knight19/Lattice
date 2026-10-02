package wal

import (
	stdErrors "errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/metrics"
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
// A nil ReplayFunc is a no-op returning nil, consistent with the
// validation-only mode of passing a nil ReplaySink to RecoverWAL
// (AUDIT re-audit: nil func values would otherwise panic on call).
func (f ReplayFunc) Apply(rec Record) error {
	if f == nil {
		return nil
	}
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

	// SequenceGaps is the number of observed discontinuities where a record's SeqNum
	// exceeded the previous record's by more than one.
	//
	// Informational only, and deliberately not an error. Gaps are legitimately produced
	// by a write that consumed a sequence number and then failed to persist, by a
	// partially written batch, and by the post-recovery watermark being seeded from
	// max(manifest checkpoint, WAL last SeqNum). Enforcing contiguity would therefore
	// refuse to open databases that are perfectly healthy. See docs/recovery-spec.md 4.2.
	SequenceGaps int

	// SkippedSequenceNumbers is the total number of sequence numbers absent from the
	// WAL across all observed gaps. It is an observability signal, not an integrity
	// verdict: a non-zero value does not by itself indicate data loss.
	SkippedSequenceNumbers uint64
}

// observeSequenceGap records a discontinuity between prev and cur without rejecting
// it. Recovery tolerates gaps because they are produced by legitimate write-failure
// paths; making them fatal would break healthy databases.
func (r *RecoveryReport) observeSequenceGap(prev, cur binary.SeqNum) {
	// Guard the subtraction: cur > prev is guaranteed by the caller's monotonicity
	// check, but the unsigned wrap is cheap to rule out.
	if cur <= prev {
		return
	}
	skipped := uint64(cur - prev - 1)
	if skipped == 0 {
		// Contiguous: prev+1. Not a gap.
		return
	}
	r.SequenceGaps++
	r.SkippedSequenceNumbers += skipped
	metrics.WALSequenceGaps.Add(1)
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
//
// RecoverWAL coordinates multi-segment crash recovery across a database WAL directory.
func RecoverWAL(dbPath string, sink ReplaySink) (RecoveryReport, error) {
	return RecoverWALFrom(dbPath, sink, 1)
}

// RecoverWALFrom coordinates multi-segment crash recovery expecting the first segment ID to be expectedStartID.
// If expectedStartID is 0, DefaultInitialSegmentID (1) is used.
func RecoverWALFrom(dbPath string, sink ReplaySink, expectedStartID uint64) (RecoveryReport, error) {
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
	if err := ValidateSegmentContinuityFrom(ids, expectedStartID); err != nil {
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

		// Load the seal-time attestation. Absent is tolerated for pre-attestation
		// databases; a mismatch is fatal, because a historical segment is immutable
		// and any divergence means whole records were lost.
		histAtt, attErr := ReadAttestation(cleanDBPath, histID)
		hasHistAtt := attErr == nil
		switch {
		case attErr == nil:
		case stdErrors.Is(attErr, errors.ErrAttestationAbsent):
			metrics.WALAttestationAbsent.Add(1)
		case stdErrors.Is(attErr, errors.ErrAttestationCorrupted):
			return report, fmt.Errorf("wal: historical segment %d attestation is unusable: %w", histID, attErr)
		default:
			return report, fmt.Errorf("wal: failed to read historical segment %d attestation: %w", histID, attErr)
		}

		reader, err := OpenSegmentReader(cleanDBPath, histID)
		if err != nil {
			return report, fmt.Errorf("wal: failed to open historical segment %d: %w", histID, err)
		}
		var (
			histRecords uint64
			histLastSeq binary.SeqNum
		)
		for {
			rec, nextErr := reader.Next()
			if nextErr == nil {
				histRecords++
				if rec.SeqNum > histLastSeq {
					histLastSeq = rec.SeqNum
				}
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
					// Gaps are NOT observed here. The historical pass and the replay
					// pass both traverse segments 1..N-1, so counting in both would
					// double-count every gap. The replay pass covers all segments and
					// is the single place gaps are observed.
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

		// Attestation check for the now-fully-scanned historical segment. reader.Offset()
		// is the exact physical length of the valid prefix, and the loop above only
		// breaks on a clean io.EOF, so offset == segment length here.
		if hasHistAtt {
			if vErr := VerifyAttestation(histID, &histAtt, histRecords, reader.Offset()); vErr != nil {
				_ = reader.Close()
				metrics.WALAttestationMismatches.Add(1)
				return report, vErr
			}
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

	// Verify the active segment's attestation BEFORE recovering it. The sidecar was
	// written at the previous Close (unsealed) or at rotation (sealed), so it proves
	// the segment has not already lost records before this recovery run. Doing it
	// first matters: RecoverSegment truncates a torn tail, which would otherwise
	// silently erase the evidence of a boundary-aligned truncation that happened
	// earlier.
	latestAtt, latErr := ReadAttestation(cleanDBPath, latestID)
	hasLatestAtt := latErr == nil
	switch {
	case latErr == nil:
	case stdErrors.Is(latErr, errors.ErrAttestationAbsent):
		metrics.WALAttestationAbsent.Add(1)
	case stdErrors.Is(latErr, errors.ErrAttestationCorrupted):
		return report, fmt.Errorf("wal: latest segment %d attestation is unusable: %w", latestID, latErr)
	default:
		return report, fmt.Errorf("wal: failed to read latest segment %d attestation: %w", latestID, latErr)
	}

	if hasLatestAtt {
		// Count records without mutating anything, to compare against the attestation.
		observed, obsErr := countSegmentRecords(latestPath)
		if obsErr != nil {
			return report, fmt.Errorf("wal: failed to scan latest segment %d: %w", latestID, obsErr)
		}
		// A torn tail is expected here and is handled below by RecoverSegment, so
		// compare only up to the last complete record: a smaller valid prefix than
		// attested still means whole records vanished.
		if obsErr := VerifyAttestation(latestID, &latestAtt, observed.count, observed.offset); obsErr != nil {
			metrics.WALAttestationMismatches.Add(1)
			return report, obsErr
		}
	}

	recRes, recErr := RecoverSegment(latestPath)
	if recErr != nil {
		report.Truncated = recRes.Truncated
		return report, fmt.Errorf("wal: failed to recover latest segment %d: %w", latestID, recErr)
	}

	if recRes.Truncated {
		report.Truncated = true
		report.TruncatedBytes = initialLatestSize - recRes.RecoveredOffset
	}
	// Retained for the attestation refresh after the replay pass.
	latestValidRecords := recRes.ValidRecords
	latestValidOffset := recRes.RecoveredOffset

	// The active segment's attestation is refreshed at the end of this function, after
	// the replay pass has established the true last sequence number.

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
				report.observeSequenceGap(prevSeqNum, rec.SeqNum)
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

	// Refresh the active segment's attestation so the post-recovery, post-truncation
	// length becomes the new baseline. Written unsealed because OpenRotatingWriter
	// will reopen and append to this segment.
	//
	// This runs after the replay pass so lastSeqNum is the real value; writing it
	// earlier recorded 0, which left the sidecar inconsistent with the segment it
	// describes.
	refreshAtt := NewAttestation(uint64(latestValidRecords), latestValidOffset, report.LastSeqNum, false)
	if err := WriteAttestation(cleanDBPath, latestID, refreshAtt); err != nil {
		metrics.WALAttestationWriteFailures.Add(1)
	}

	return report, nil
}

// segmentScan is a read-only structural summary of a segment file.
type segmentScan struct {
	count   uint64
	offset  int64
	lastSeq binary.SeqNum
}

// countSegmentRecords walks a segment read-only and reports how many complete
// CRC-verified records it holds plus the byte offset just past the last one.
//
// It stops at the first record that does not decode cleanly (torn tail,
// corruption, or EOF) without mutating the file, so it is safe to call before
// RecoverSegment. Because the scan stops early, count and offset describe the
// complete valid prefix, which is exactly what an attestation compares against.
func countSegmentRecords(path string) (segmentScan, error) {
	var out segmentScan
	f, err := openFileNoFollow(filepath.Clean(path), os.O_RDONLY, 0)
	if err != nil {
		return out, err
	}
	defer func() { _ = f.Close() }()

	for {
		rec, derr := DecodeRecord(f)
		if derr != nil {
			// Clean EOF, a torn tail, or corruption all end the scan here.
			return out, nil
		}
		recLen := int64(MinRecordSize + len(rec.Key) + len(rec.Value))
		if out.offset > math.MaxInt64-recLen {
			return out, fmt.Errorf("wal: segment scan offset overflows int64")
		}
		out.offset += recLen
		out.count++
		if rec.SeqNum > out.lastSeq {
			out.lastSeq = rec.SeqNum
		}
	}
}

// ValidateSegmentContinuity validates that a sorted slice of segment IDs has no duplicates and no gaps,
// and enforces that a non-empty segment chain begins at initial segment ID 1 (P07-SEC-013).
func ValidateSegmentContinuity(ids []uint64) error {
	return ValidateSegmentContinuityFrom(ids, 1)
}

// ValidateSegmentContinuityFrom validates that a sorted slice of segment IDs has no duplicates and no gaps,
// and enforces that a non-empty segment chain begins at expectedStart.
// If expectedStart <= 0, default initial segment ID 1 is used.
func ValidateSegmentContinuityFrom(ids []uint64, expectedStart uint64) error {
	if len(ids) == 0 {
		return nil
	}
	if expectedStart <= 0 {
		expectedStart = 1
	}
	if ids[0] != expectedStart {
		return &errors.SegmentGapError{
			Expected: expectedStart,
			Actual:   ids[0],
		}
	}
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
