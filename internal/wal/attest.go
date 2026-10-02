package wal

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/security"
)

// Segment attestation.
//
// Per-record CRC32 protects each record against accidental corruption, but it
// cannot detect the loss of whole records. A segment tail removed exactly at a
// record boundary decodes as a clean io.EOF, so recovery sees a shorter but
// perfectly valid log and reports success. There is no segment footer, record
// count, final LSN, or chained checksum anywhere in this package to catch it.
//
// A sidecar file records what the segment contained when it was last sealed (or
// last observed). Comparing that against what is actually on disk turns a
// boundary-aligned truncation from undetectable into a hard failure.
//
// Format (36 bytes, big-endian, CRC32-IEEE protected):
//
//	offset  size  field
//	     0     4  magic
//	     4     2  version
//	     6     2  flags (bit 0 = sealed)
//	     8     8  recordCount
//	    16     8  finalOffset (physical bytes of the segment when attested)
//	    24     8  lastSeqNum
//	    32     4  CRC32-IEEE over bytes [0:32)
//
// recordCount, finalOffset, and lastSeqNum are all full 64-bit fields. SeqNum is a
// uint64 across this codebase, so narrowing lastSeqNum would cap attestation at
// 2^32 and silently mis-verify exactly the large databases that most need it.
//
// The sidecar lives beside the segment as wal_<id>.att. ListSegments skips
// non-conforming names, so an .att file is never mistaken for a segment, and the
// engine's orphan cleaner only scans direct children of the database directory,
// never the wal/ subdirectory.

const (
	// AttestationMagic identifies a segment attestation sidecar.
	AttestationMagic uint32 = 0x5741_5454 // "WATT"

	// AttestationVersion is the current attestation layout version.
	AttestationVersion uint16 = 1

	// AttestationHeaderSize is the number of attested bytes covered by the CRC.
	AttestationHeaderSize = 32

	// AttestationRecordSize is the total sidecar size including the trailing CRC.
	AttestationRecordSize = AttestationHeaderSize + 4

	// AttestationSuffix is the sidecar file extension.
	AttestationSuffix = ".att"

	// AttestationTmpSuffix is the staging suffix used for crash-safe writes.
	AttestationTmpSuffix = ".att.tmp"

	// FlagSealed marks a segment that can never grow again. Its attested length
	// is final, so any divergence is corruption rather than normal growth.
	FlagSealed uint16 = 1 << 0
)

// AttestationSuffixName returns the sidecar filename for a segment ID.
func AttestationFilename(id uint64) string {
	return SegmentName(id) + AttestationSuffix
}

// AttestationPath returns the platform-aware sidecar path for a segment ID.
func AttestationPath(dbPath string, id uint64) string {
	return filepath.Join(Dir(dbPath), AttestationFilename(id))
}

// Attestation is the decoded attestation record for one segment.
//
// It captures the three facts that, together, pin a segment's contents at a point
// in time: how many records it held, how many physical bytes they occupied, and
// the highest sequence number among them.
type Attestation struct {
	// Version is the layout version that produced this record.
	Version uint16
	// Sealed reports whether the segment was final when attested. A sealed
	// segment must never change again.
	Sealed bool
	// RecordCount is the number of complete CRC-verified records observed.
	RecordCount uint64
	// FinalOffset is the physical byte length of the segment when attested.
	FinalOffset uint64
	// LastSeqNum is the highest sequence number observed, or 0 when empty.
	LastSeqNum binary.SeqNum
}

// NewAttestation builds an attestation from observed segment facts.
func NewAttestation(recordCount uint64, finalOffset int64, lastSeqNum binary.SeqNum, sealed bool) Attestation {
	off := uint64(0)
	if finalOffset > 0 {
		off = uint64(finalOffset)
	}
	flags := uint16(0)
	if sealed {
		flags |= FlagSealed
	}
	return Attestation{
		Version:     AttestationVersion,
		Sealed:      sealed,
		RecordCount: recordCount,
		FinalOffset: off,
		LastSeqNum:  lastSeqNum,
	}
}

// Encode serializes the attestation into its fixed 32-byte layout.
func (a Attestation) Encode() []byte {
	buf := make([]byte, AttestationRecordSize)
	binary.PutUint32(buf[0:4], AttestationMagic)
	binary.PutUint16(buf[4:6], AttestationVersion)
	flags := uint16(0)
	if a.Sealed {
		flags |= FlagSealed
	}
	binary.PutUint16(buf[6:8], flags)
	// 64-bit fields are written big-endian across 8 bytes. Truncating them to a
	// single uint32 would silently lose everything above 2^32, which a large
	// segment length or sequence watermark can legitimately exceed.
	binary.PutUint64(buf[8:16], a.RecordCount)
	binary.PutUint64(buf[16:24], a.FinalOffset)
	binary.PutUint64(buf[24:32], uint64(a.LastSeqNum))
	binary.PutUint32(buf[32:36], binary.Checksum(buf[0:AttestationHeaderSize]))
	return buf
}

// DecodeAttestation parses and validates a sidecar, returning ErrAttestationCorrupted
// on any structural, magic, version, or CRC failure. Callers distinguish "absent"
// via errors.Is(err, errors.ErrAttestationAbsent), which DecodeAttestation never
// returns.
func DecodeAttestation(data []byte) (Attestation, error) {
	if len(data) != AttestationRecordSize {
		return Attestation{}, fmt.Errorf("%w: attestation is %d bytes, want %d",
			errors.ErrAttestationCorrupted, len(data), AttestationRecordSize)
	}
	if magic := binary.GetUint32(data[0:4]); magic != AttestationMagic {
		return Attestation{}, fmt.Errorf("%w: bad magic 0x%08x, want 0x%08x",
			errors.ErrAttestationCorrupted, magic, AttestationMagic)
	}
	version := binary.GetUint16(data[4:6])
	if version != AttestationVersion {
		return Attestation{}, fmt.Errorf("%w: unsupported attestation version %d",
			errors.ErrAttestationCorrupted, version)
	}
	if err := binary.VerifyChecksum(data[0:AttestationHeaderSize], binary.GetUint32(data[32:36])); err != nil {
		return Attestation{}, fmt.Errorf("%w: %v", errors.ErrAttestationCorrupted, err)
	}
	flags := binary.GetUint16(data[6:8])
	if flags&^FlagSealed != 0 {
		return Attestation{}, fmt.Errorf("%w: unknown attestation flags 0x%04x",
			errors.ErrAttestationCorrupted, flags)
	}
	return Attestation{
		Version:     version,
		Sealed:      flags&FlagSealed != 0,
		RecordCount: binary.GetUint64(data[8:16]),
		FinalOffset: binary.GetUint64(data[16:24]),
		LastSeqNum:  binary.SeqNum(binary.GetUint64(data[24:32])),
	}, nil
}

// ReadAttestation loads a segment's sidecar.
//
// It returns an error wrapping errors.ErrAttestationAbsent when no sidecar exists,
// which is the expected state for a database created before attestation existed and
// for a segment that has never been sealed.
func ReadAttestation(dbPath string, id uint64) (Attestation, error) {
	path := AttestationPath(dbPath, id)
	cleanPath, err := security.CleanAndValidatePath(path)
	if err != nil {
		return Attestation{}, fmt.Errorf("wal: invalid attestation path: %w", err)
	}
	data, err := os.ReadFile(cleanPath)
	if err != nil {
		if os.IsNotExist(err) {
			return Attestation{}, fmt.Errorf("wal: no attestation for segment %d: %w",
				id, errors.ErrAttestationAbsent)
		}
		return Attestation{}, fmt.Errorf("wal: failed to read attestation for segment %d: %w", id, err)
	}
	att, err := DecodeAttestation(data)
	if err != nil {
		return Attestation{}, fmt.Errorf("wal: attestation for segment %d is invalid: %w", id, err)
	}
	return att, nil
}

// WriteAttestation durably records a segment's attested facts.
//
// The write is crash-safe: staging file, fsync, atomic rename, parent directory
// fsync. A failure before the rename leaves the previous sidecar untouched, which
// is safe because the caller only attests facts it has already made durable in the
// segment itself.
//
// When sealed is true the segment is final and its attested length becomes an
// invariant: any later divergence is reported by VerifyAttestation. When false the
// segment is still the active one and may legitimately grow, so only a shrink is
// treated as loss.
func WriteAttestation(dbPath string, id uint64, att Attestation) error {
	if att.Version == 0 {
		att.Version = AttestationVersion
	}
	walDir := Dir(dbPath)
	if err := os.MkdirAll(walDir, DirMode); err != nil {
		return fmt.Errorf("wal: failed to create wal directory for attestation: %w", err)
	}
	tmpPath := filepath.Join(walDir, AttestationFilename(id)+AttestationTmpSuffix)
	finalPath := AttestationPath(dbPath, id)

	_ = os.Remove(tmpPath)
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, FileMode)
	if err != nil {
		return fmt.Errorf("wal: failed to stage attestation for segment %d: %w", id, err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = f.Close()
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := f.Write(att.Encode()); err != nil {
		return fmt.Errorf("wal: failed to write attestation for segment %d: %w", id, err)
	}
	if err := fdatasync(f); err != nil {
		return fmt.Errorf("wal: failed to sync attestation for segment %d: %w", id, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("wal: failed to close attestation for segment %d: %w", id, err)
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return fmt.Errorf("wal: failed to publish attestation for segment %d: %w", id, err)
	}
	ok = true
	if err := SyncDir(walDir); err != nil {
		return fmt.Errorf("wal: failed to sync wal directory after attesting segment %d: %w", id, err)
	}
	return nil
}

// VerifyAttestation compares what is actually on disk against a segment's
// attestation.
//
// observed must describe the segment as it exists now: the number of complete
// CRC-verified records and the physical byte offset immediately after the last one.
// An empty segment legitimately observes zero.
//
// Decision table:
//
//   - attested == nil: no attestation exists. This is a pre-attestation database
//     or a segment never sealed, so there is nothing to compare against. Reported
//     as absent so callers can choose a policy; it is never treated as a pass.
//   - sealed and anything differs: corruption. The segment was declared final and
//     has changed.
//   - not sealed and the segment shrank: corruption. A segment that is still active
//     may grow but must never lose bytes.
//   - not sealed and the segment grew: expected. The sidecar is stale and should be
//     refreshed by the caller.
//   - identical: pass.
//
// recordCount and finalOffset mismatches are both reported so an operator can see
// whether whole records vanished or the tail was cut mid-record.
func VerifyAttestation(id uint64, att *Attestation, recordCount uint64, finalOffset int64) error {
	if att == nil {
		return fmt.Errorf("wal: segment %d has no attestation: %w", id, errors.ErrAttestationAbsent)
	}
	observed := uint64(0)
	if finalOffset > 0 {
		observed = uint64(finalOffset)
	}

	shrunk := observed < att.FinalOffset
	countMismatch := recordCount != att.RecordCount
	offsetMismatch := observed != att.FinalOffset
	_ = countMismatch

	if att.Sealed {
		if countMismatch || offsetMismatch {
			return fmt.Errorf("%w: sealed segment %d diverged from its attestation "+
				"(attested %d records / %d bytes, observed %d records / %d bytes); "+
				"a sealed segment is immutable and this indicates whole-record loss or "+
				"out-of-band modification",
				errors.ErrAttestationMismatch, id,
				att.RecordCount, att.FinalOffset, recordCount, observed)
		}
		return nil
	}

	// An active segment is reopened and appended to, so it may legitimately grow.
	// Only losing records or bytes is a problem. Comparing raw equality here would
	// reject every append, since growth changes both counters.
	if shrunk || recordCount < att.RecordCount {
		return fmt.Errorf("%w: active segment %d lost data relative to its attestation "+
			"(attested %d records / %d bytes, observed %d records / %d bytes)",
			errors.ErrAttestationMismatch, id,
			att.RecordCount, att.FinalOffset, recordCount, observed)
	}
	// Growth is legitimate for the active segment; the caller refreshes the sidecar.
	return nil
}
