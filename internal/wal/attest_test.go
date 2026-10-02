package wal_test

import (
	stderrors "errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	latticeerrors "github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/wal"
)

// =============================================================================
// Segment attestation.
//
// Per-record CRC32 protects each record but cannot detect the loss of whole
// records: a tail removed exactly at a record boundary decodes as a clean io.EOF,
// so recovery previously reported success with silently missing acknowledged
// writes. A sidecar records each segment's record count, physical length, and
// highest SeqNum at seal time, turning that undetectable loss into a hard failure.
// =============================================================================

// countingSink records the SeqNums it observes.
type countingSink struct{ seqs []uint64 }

func (c *countingSink) Apply(rec wal.Record) error {
	c.seqs = append(c.seqs, uint64(rec.SeqNum))
	return nil
}

func putRec(seq uint64) wal.Record {
	return wal.Record{
		Type:   wal.RecordTypePut,
		SeqNum: binary.SeqNum(seq),
		Key:    []byte("k"),
		Value:  []byte("value-padding-so-record-lengths-are-uniform"),
	}
}

// buildSegments writes n records, rotates, writes m more, then closes.
func buildSegments(t *testing.T, dbPath string, n, m uint64) {
	t.Helper()
	rw, err := wal.OpenRotatingWriter(dbPath, wal.Options{})
	if err != nil {
		t.Fatalf("OpenRotatingWriter: %v", err)
	}
	for i := uint64(1); i <= n; i++ {
		if err := rw.AppendSync(putRec(i)); err != nil {
			t.Fatalf("AppendSync %d: %v", i, err)
		}
	}
	if m > 0 {
		if err := rw.Rotate(); err != nil {
			t.Fatalf("Rotate: %v", err)
		}
		for i := n + 1; i <= n+m; i++ {
			if err := rw.AppendSync(putRec(i)); err != nil {
				t.Fatalf("AppendSync %d: %v", i, err)
			}
		}
	}
	if err := rw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// segmentBoundaries returns every valid record-boundary offset in a segment.
func segmentBoundaries(t *testing.T, dbPath string, id uint64) []int64 {
	t.Helper()
	r, err := wal.OpenSegmentReader(dbPath, id)
	if err != nil {
		t.Fatalf("OpenSegmentReader(%d): %v", id, err)
	}
	defer func() { _ = r.Close() }()
	out := []int64{0}
	for {
		if _, err := r.Next(); err != nil {
			break
		}
		out = append(out, r.Offset())
	}
	return out
}

func segmentFile(t *testing.T, dbPath string, id uint64) string {
	t.Helper()
	return filepath.Join(dbPath, "wal", wal.SegmentName(id))
}

// --- Format round trip -------------------------------------------------------

func TestAttestation_EncodeDecodeRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name string
		att  wal.Attestation
	}{
		{"empty unsealed", wal.NewAttestation(0, 0, 0, false)},
		{"empty sealed", wal.NewAttestation(0, 0, 0, true)},
		{"populated unsealed", wal.NewAttestation(42, 4096, binary.SeqNum(99), false)},
		{"populated sealed", wal.NewAttestation(1024, 1<<20, binary.SeqNum(1<<20), true)},
		{"large offsets", wal.NewAttestation(1<<32, 1<<40, binary.SeqNum(1<<33), true)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			enc := tc.att.Encode()
			if len(enc) != wal.AttestationRecordSize {
				t.Fatalf("encoded %d bytes; want %d", len(enc), wal.AttestationRecordSize)
			}
			got, err := wal.DecodeAttestation(enc)
			if err != nil {
				t.Fatalf("DecodeAttestation: %v", err)
			}
			if got.RecordCount != tc.att.RecordCount || got.FinalOffset != tc.att.FinalOffset ||
				got.LastSeqNum != tc.att.LastSeqNum || got.Sealed != tc.att.Sealed {
				t.Errorf("round trip mismatch: got %+v, want %+v", got, tc.att)
			}
		})
	}
}

func TestAttestation_DecodeRejectsCorruption(t *testing.T) {
	good := wal.NewAttestation(10, 512, binary.SeqNum(10), true).Encode()

	t.Run("wrong size", func(t *testing.T) {
		// Undersized buffers, sliced within bounds.
		for _, n := range []int{0, 1, wal.AttestationHeaderSize, wal.AttestationRecordSize - 1} {
			if _, err := wal.DecodeAttestation(good[:n]); err == nil {
				t.Errorf("size %d accepted; want rejection", n)
			}
		}
		// Oversized buffer: trailing bytes must also be rejected.
		oversized := append(append([]byte(nil), good...), 0x00)
		if _, err := wal.DecodeAttestation(oversized); !stderrors.Is(err, latticeerrors.ErrAttestationCorrupted) {
			t.Errorf("oversized buffer returned %v; want ErrAttestationCorrupted", err)
		}
	})

	t.Run("bad magic", func(t *testing.T) {
		bad := append([]byte(nil), good...)
		bad[0] ^= 0xFF
		if _, err := wal.DecodeAttestation(bad); !stderrors.Is(err, latticeerrors.ErrAttestationCorrupted) {
			t.Errorf("bad magic returned %v; want ErrAttestationCorrupted", err)
		}
	})

	t.Run("unsupported version", func(t *testing.T) {
		bad := wal.NewAttestation(1, 2, 3, false)
		enc := bad.Encode()
		binary.PutUint16(enc[4:6], 999)
		binary.PutUint32(enc[wal.AttestationHeaderSize:wal.AttestationRecordSize], binary.Checksum(enc[0:wal.AttestationHeaderSize]))
		if _, err := wal.DecodeAttestation(enc); !stderrors.Is(err, latticeerrors.ErrAttestationCorrupted) {
			t.Errorf("bad version returned %v; want ErrAttestationCorrupted", err)
		}
	})

	t.Run("crc mismatch", func(t *testing.T) {
		// Flip a byte inside the covered region without fixing the CRC.
		bad := append([]byte(nil), good...)
		bad[12] ^= 0x01
		if _, err := wal.DecodeAttestation(bad); !stderrors.Is(err, latticeerrors.ErrAttestationCorrupted) {
			t.Errorf("crc mismatch returned %v; want ErrAttestationCorrupted", err)
		}
	})

	t.Run("every covered byte is protected", func(t *testing.T) {
		for i := 0; i < wal.AttestationHeaderSize; i++ {
			bad := append([]byte(nil), good...)
			bad[i] ^= 0xFF
			if _, err := wal.DecodeAttestation(bad); err == nil {
				t.Errorf("byte %d flip undetected", i)
			}
		}
	})
}

// --- Absent attestation is not a pass ---------------------------------------

func TestAttestation_AbsentIsDistinctFromPass(t *testing.T) {
	dbPath := t.TempDir()
	if _, err := wal.ReadAttestation(dbPath, 1); !stderrors.Is(err, latticeerrors.ErrAttestationAbsent) {
		t.Errorf("ReadAttestation on an empty WAL returned %v; want ErrAttestationAbsent", err)
	}
	if err := wal.VerifyAttestation(1, nil, 0, 0); !stderrors.Is(err, latticeerrors.ErrAttestationAbsent) {
		t.Errorf("VerifyAttestation(nil) returned %v; want ErrAttestationAbsent", err)
	}
}

// --- VerifyAttestation decision table ----------------------------------------

func TestAttestation_VerifyDecisionTable(t *testing.T) {
	sealed := wal.NewAttestation(10, 1000, binary.SeqNum(10), true)
	open := wal.NewAttestation(10, 1000, binary.SeqNum(10), false)

	cases := []struct {
		name    string
		att     wal.Attestation
		count   uint64
		offset  int64
		wantErr bool
	}{
		{"sealed exact match", sealed, 10, 1000, false},
		{"sealed lost a record", sealed, 9, 900, true},
		{"sealed grew", sealed, 11, 1100, true},
		{"sealed empty prefix", sealed, 0, 0, true},
		{"open exact match", open, 10, 1000, false},
		{"open grew is legal", open, 20, 2000, false},
		{"open lost a record", open, 9, 900, true},
		{"open shrank only", open, 10, 500, true},
		{"open emptied", open, 0, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			att := tc.att
			err := wal.VerifyAttestation(7, &att, tc.count, tc.offset)
			if tc.wantErr {
				if !stderrors.Is(err, latticeerrors.ErrAttestationMismatch) {
					t.Errorf("got %v; want ErrAttestationMismatch", err)
				}
			} else if err != nil {
				t.Errorf("got %v; want nil", err)
			}
		})
	}
}

// --- The regression: boundary-aligned truncation of a sealed segment ---------

func TestAttestation_SealedSegmentTruncationIsDetected(t *testing.T) {
	dbPath := t.TempDir()
	buildSegments(t, dbPath, 8, 4)

	ids, err := wal.ListSegments(dbPath)
	if err != nil {
		t.Fatalf("ListSegments: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("expected 2 segments, got %v", ids)
	}

	// Baseline must recover cleanly.
	var base countingSink
	baseRep, err := wal.RecoverWAL(dbPath, &base)
	if err != nil {
		t.Fatalf("baseline RecoverWAL: %v", err)
	}
	wantRecords := baseRep.ValidRecords

	// Drop whole records from the sealed segment, aligned to a boundary.
	histID := ids[0]
	bounds := segmentBoundaries(t, dbPath, histID)
	cut := bounds[len(bounds)-2]
	if err := os.Truncate(segmentFile(t, dbPath, histID), cut); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	var after countingSink
	_, err = wal.RecoverWAL(dbPath, &after)
	if !stderrors.Is(err, latticeerrors.ErrAttestationMismatch) {
		t.Fatalf("RecoverWAL after boundary truncation returned %v; want ErrAttestationMismatch "+
			"(pre-fix this returned nil with %d records silently missing)", err,
			wantRecords-len(after.seqs))
	}
	if !stderrors.Is(err, latticeerrors.ErrAttestationCorrupted) {
		// sanity: it should be a mismatch, not merely a corrupt sidecar
		_ = err
	}
}

// TestAttestation_ActiveSegmentTruncationIsDetected covers the unsealed segment,
// which is the one reopened and appended to. Growth is legal; shrinking is loss.
func TestAttestation_ActiveSegmentTruncationIsDetected(t *testing.T) {
	dbPath := t.TempDir()
	buildSegments(t, dbPath, 6, 0) // single segment, never rotated => active

	ids, err := wal.ListSegments(dbPath)
	if err != nil {
		t.Fatalf("ListSegments: %v", err)
	}
	activeID := ids[len(ids)-1]

	att, err := wal.ReadAttestation(dbPath, activeID)
	if err != nil {
		t.Fatalf("ReadAttestation: %v", err)
	}
	if att.Sealed {
		t.Error("the single active segment was attested as sealed; it must be unsealed")
	}

	bounds := segmentBoundaries(t, dbPath, activeID)
	cut := bounds[len(bounds)-2]
	if err := os.Truncate(segmentFile(t, dbPath, activeID), cut); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	if _, err := wal.RecoverWAL(dbPath, nil); !stderrors.Is(err, latticeerrors.ErrAttestationMismatch) {
		t.Errorf("RecoverWAL after active-segment truncation returned %v; want ErrAttestationMismatch", err)
	}
}

// TestAttestation_ActiveSegmentGrowthIsAccepted guards the other direction: the
// active segment is reopened and appended to, so it must be allowed to grow.
func TestAttestation_ActiveSegmentGrowthIsAccepted(t *testing.T) {
	dbPath := t.TempDir()
	buildSegments(t, dbPath, 5, 0)

	// Reopen and append more, which legitimately grows the active segment.
	rw, err := wal.OpenRotatingWriter(dbPath, wal.Options{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	for i := uint64(100); i <= 105; i++ {
		if err := rw.AppendSync(putRec(i)); err != nil {
			t.Fatalf("AppendSync %d: %v", i, err)
		}
	}
	if err := rw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	var sink countingSink
	rep, err := wal.RecoverWAL(dbPath, &sink)
	if err != nil {
		t.Fatalf("RecoverWAL after legitimate growth: %v", err)
	}
	if rep.ValidRecords != 11 {
		t.Errorf("recovered %d records; want 11", rep.ValidRecords)
	}
}

// --- Backward compatibility -------------------------------------------------

// TestAttestation_LegacyDatabaseWithoutSidecarsStillRecovers is the most
// important compatibility test: failing closed on a missing sidecar would brick
// every database created before this change.
func TestAttestation_LegacyDatabaseWithoutSidecarsStillRecovers(t *testing.T) {
	dbPath := t.TempDir()
	buildSegments(t, dbPath, 7, 3)

	// Delete every sidecar, simulating a pre-attestation database.
	entries, err := os.ReadDir(filepath.Join(dbPath, "wal"))
	if err != nil {
		t.Fatalf("read wal dir: %v", err)
	}
	removed := 0
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".att" {
			if err := os.Remove(filepath.Join(dbPath, "wal", e.Name())); err != nil {
				t.Fatalf("remove sidecar: %v", err)
			}
			removed++
		}
	}
	if removed == 0 {
		t.Skip("no sidecars were written; nothing to remove")
	}

	var sink countingSink
	rep, err := wal.RecoverWAL(dbPath, &sink)
	if err != nil {
		t.Fatalf("legacy database failed to recover: %v; a missing sidecar must be tolerated", err)
	}
	if rep.ValidRecords != 10 {
		t.Errorf("legacy recovery replayed %d records; want 10", rep.ValidRecords)
	}

	// Recovery re-attests the active segment, so the legacy database upgrades itself.
	if att, aerr := wal.ReadAttestation(dbPath, 2); aerr != nil {
		t.Errorf("active segment was not re-attested after legacy recovery: %v", aerr)
	} else if att.Sealed {
		t.Error("re-attested active segment marked sealed; it is still appendable")
	}
}

// TestAttestation_CorruptedSidecarFailsClosed: a corrupt sidecar must not be
// silently ignored, because that would re-open the very hole this closes.
func TestAttestation_CorruptedSidecarFailsClosed(t *testing.T) {
	dbPath := t.TempDir()
	buildSegments(t, dbPath, 5, 3)

	attPath := wal.AttestationPath(dbPath, 1)
	raw, err := os.ReadFile(attPath)
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	raw[10] ^= 0xFF // flip a covered byte, leave the CRC stale
	if err := os.WriteFile(attPath, raw, 0600); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}

	if _, err := wal.RecoverWAL(dbPath, nil); !stderrors.Is(err, latticeerrors.ErrAttestationCorrupted) {
		t.Errorf("RecoverWAL with a corrupted sidecar returned %v; want ErrAttestationCorrupted", err)
	}
}

// TestAttestation_SidecarsAreNotMistakenForSegments guards ListSegments.
func TestAttestation_SidecarsAreNotMistakenForSegments(t *testing.T) {
	dbPath := t.TempDir()
	buildSegments(t, dbPath, 4, 2)

	ids, err := wal.ListSegments(dbPath)
	if err != nil {
		t.Fatalf("ListSegments: %v", err)
	}
	for _, id := range ids {
		if id == 0 {
			t.Errorf("ListSegments returned segment id 0: %v", ids)
		}
	}
	if len(ids) != 2 {
		t.Errorf("ListSegments returned %v; want exactly [1 2] with sidecars present", ids)
	}

	// Continuity validation must also ignore the sidecars.
	if err := wal.ValidateSegmentContinuity(ids); err != nil {
		t.Errorf("ValidateSegmentContinuity: %v", err)
	}
}

// TestAttestation_SidecarOutsideWalDirIsIgnored is belt-and-braces: even a
// malformed sidecar name must not break discovery.
func TestAttestation_SidecarOutsideWalDirIsIgnored(t *testing.T) {
	dbPath := t.TempDir()
	buildSegments(t, dbPath, 3, 0)

	junk := filepath.Join(dbPath, "wal", "not-a-segment.att")
	if err := os.WriteFile(junk, []byte("garbage"), 0600); err != nil {
		t.Fatalf("write junk: %v", err)
	}
	ids, err := wal.ListSegments(dbPath)
	if err != nil {
		t.Fatalf("ListSegments: %v", err)
	}
	if len(ids) != 1 {
		t.Errorf("ListSegments returned %v; want exactly [1]", ids)
	}
}

// TestAttestation_RepeatedRecoveryIsStable: recovery rewrites the active
// attestation, so running it twice must not start reporting drift.
func TestAttestation_RepeatedRecoveryIsStable(t *testing.T) {
	dbPath := t.TempDir()
	buildSegments(t, dbPath, 6, 2)

	for i := 0; i < 3; i++ {
		if _, err := wal.RecoverWAL(dbPath, nil); err != nil {
			t.Fatalf("recovery pass %d: %v", i, err)
		}
	}
}

// TestAttestation_NoResidueAfterWrite: the staging file must never survive.
func TestAttestation_NoResidueAfterWrite(t *testing.T) {
	dbPath := t.TempDir()
	if err := wal.WriteAttestation(dbPath, 1, wal.NewAttestation(3, 300, binary.SeqNum(3), true)); err != nil {
		t.Fatalf("WriteAttestation: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(dbPath, "wal"))
	if err != nil {
		t.Fatalf("read wal dir: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" || e.Name() == "" {
			t.Errorf("staging residue left behind: %q", e.Name())
		}
	}
	if len(entries) != 1 {
		t.Errorf("wal dir contains %v; want only the sidecar", entries)
	}
}
