package wal_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/wal"
)

// SEC-03.11: Persistence Boundary Fuzz Testing
//
// Fuzz targets:
//   1. FuzzRecordDecoderPersistence: Record decoder robustness on arbitrary byte sequences.
//   2. FuzzRecordHeaderDecoder: Header decoder bounds and parsing.
//   3. FuzzWALReaderFromBytes: Reader loop termination, bounded iterations, and validity.
//   4. FuzzRecoveryCoordinator: Multi-segment crash recovery over arbitrary filesystem payloads.
//   5. FuzzSegmentNameParsing: Filename parsing boundary validation.

// FuzzRecordDecoderPersistence exercises the record decoder with arbitrary byte slices.
// Security Invariant: arbitrary bytes must either be rejected with an error or decode to a valid,
// checksum-verified record; never panic or cause uncontrolled loops.
func FuzzRecordDecoderPersistence(f *testing.F) {
	// Seed with valid and boundary records
	seeds := [][]byte{
		{},
		{0, 0, 0, 0},
		make([]byte, wal.MinRecordSize),
		make([]byte, wal.HeaderSize),
	}
	validRec := testRecord(1, "seed-key", "seed-val")
	if encoded, err := wal.EncodeRecord(validRec); err == nil {
		seeds = append(seeds, encoded)
	}
	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		r := bytes.NewReader(data)
		rec, err := wal.DecodeRecord(r)
		if err == nil {
			// If decoding succeeded, the record MUST be valid
			if valErr := rec.Validate(); valErr != nil {
				t.Fatalf("SECURITY VIOLATION: DecodeRecord returned invalid record without error: %v", valErr)
			}
			// Checksum MUST match
			encoded, encErr := wal.EncodeRecord(rec)
			if encErr != nil {
				t.Fatalf("failed to encode decoded record: %v", encErr)
			}
			expectedCRC := binary.Checksum(encoded[4:])
			if binary.GetUint32(encoded[0:4]) != expectedCRC {
				t.Fatalf("SECURITY VIOLATION: CRC verification failed on decoded record")
			}
		}
	})
}

// FuzzRecordHeaderDecoder exercises header decoding against malformed inputs.
func FuzzRecordHeaderDecoder(f *testing.F) {
	f.Add([]byte{})
	f.Add(make([]byte, wal.HeaderSize))
	validRec := testRecord(42, "k", "v")
	if encoded, err := wal.EncodeRecord(validRec); err == nil {
		f.Add(encoded[:wal.HeaderSize])
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		hdr, err := wal.DecodeHeader(data)
		if err == nil {
			// If header parsed, Type must be valid
			if hdr.Type != wal.RecordTypePut &&
				hdr.Type != wal.RecordTypeDelete &&
				hdr.Type != wal.RecordTypeBatchStart &&
				hdr.Type != wal.RecordTypeBatchCommit {
				t.Fatalf("SECURITY VIOLATION: DecodeHeader parsed invalid RecordType: %v", hdr.Type)
			}
		}
	})
}

// FuzzWALReaderFromBytes writes arbitrary byte sequences to a segment file and iterates Next().
// Security Invariant: no panics, bounded loop iterations, all returned records must be strictly valid.
func FuzzWALReaderFromBytes(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte("random-garbage-bytes-not-a-record"))
	rec1 := testRecord(1, "key1", "val1")
	rec2 := testRecord(2, "key2", "val2")
	enc1, _ := wal.EncodeRecord(rec1)
	enc2, _ := wal.EncodeRecord(rec2)
	f.Add(append(enc1, enc2...))

	f.Fuzz(func(t *testing.T, data []byte) {
		// Enforce size limit on fuzz data to prevent host memory exhaustion
		if len(data) > 256*1024 {
			return
		}

		tmpDir := t.TempDir()
		segPath := filepath.Join(tmpDir, "wal_000000000001.log")
		if err := os.WriteFile(segPath, data, 0600); err != nil {
			t.Fatalf("WriteFile failed: %v", err)
		}

		reader, err := wal.OpenReader(segPath)
		if err != nil {
			return
		}
		defer func() { _ = reader.Close() }()

		const maxIterations = 1000
		var iterations int
		var prevSeq binary.SeqNum

		for {
			iterations++
			if iterations > maxIterations {
				t.Fatalf("SECURITY VIOLATION: possible infinite loop in OpenReader.Next()")
			}

			rec, nextErr := reader.Next()
			if nextErr != nil {
				break
			}

			if valErr := rec.Validate(); valErr != nil {
				t.Fatalf("SECURITY VIOLATION: Next() returned invalid record: %v", valErr)
			}
			prevSeq = rec.SeqNum
			_ = prevSeq
		}
	})
}

// FuzzRecoveryCoordinator exercises the full RecoverWAL multi-segment coordinator against
// arbitrary segment payloads.
// Security Invariant: any replayed record must be strictly valid and sequence-monotonic.
func FuzzRecoveryCoordinator(f *testing.F) {
	f.Add([]byte{}, []byte{})
	f.Add([]byte("bad segment 1"), []byte("bad segment 2"))

	rec1 := testRecord(1, "k1", "v1")
	rec2 := testRecord(2, "k2", "v2")
	enc1, _ := wal.EncodeRecord(rec1)
	enc2, _ := wal.EncodeRecord(rec2)
	f.Add(enc1, enc2)

	f.Fuzz(func(t *testing.T, seg1Bytes, seg2Bytes []byte) {
		if len(seg1Bytes) > 128*1024 || len(seg2Bytes) > 128*1024 {
			return
		}

		dbDir := t.TempDir()
		if _, err := wal.InitDir(dbDir); err != nil {
			t.Fatalf("InitDir failed: %v", err)
		}

		seg1Path := wal.SegmentPath(dbDir, 1)
		seg2Path := wal.SegmentPath(dbDir, 2)

		if err := os.WriteFile(seg1Path, seg1Bytes, 0600); err != nil {
			t.Fatalf("WriteFile seg1 failed: %v", err)
		}
		if err := os.WriteFile(seg2Path, seg2Bytes, 0600); err != nil {
			t.Fatalf("WriteFile seg2 failed: %v", err)
		}

		var sink recordingSink
		report, err := wal.RecoverWAL(dbDir, &sink)
		if err == nil {
			// If recovery succeeded:
			// 1. All replayed records must be valid
			var prevSeq binary.SeqNum
			for i, rec := range sink.records {
				if valErr := rec.Validate(); valErr != nil {
					t.Fatalf("SECURITY VIOLATION: RecoverWAL replayed invalid record %d: %v", i, valErr)
				}
				if rec.SeqNum <= prevSeq {
					t.Fatalf("SECURITY VIOLATION: RecoverWAL replayed non-monotonic sequence number %d <= %d", rec.SeqNum, prevSeq)
				}
				prevSeq = rec.SeqNum
			}
			if report.ValidRecords != len(sink.records) {
				t.Fatalf("ValidRecords mismatch: report %d vs sink %d", report.ValidRecords, len(sink.records))
			}
		}
	})
}

// FuzzSegmentNameParsing exercises segment filename parsing.
// Security Invariant: no panics, strictly valid parsed IDs.
func FuzzSegmentNameParsing(f *testing.F) {
	seeds := []string{
		"",
		"wal_000000000001.log",
		"wal_000000000002.log",
		"wal_1.log",
		"wal_99999999999999999999.log",
		"wal_000000000000.log",
		"wal_000000000001.txt",
		"segment_000000000001.log",
		"../../../wal_000000000001.log",
		"wal_-00000000001.log",
		"wal_000000000001.log\x00",
	}
	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, name string) {
		id, err := wal.ParseSegmentID(name)
		if err == nil {
			// ID must be strictly positive
			if id == 0 {
				t.Fatalf("SECURITY VIOLATION: ParseSegmentID accepted ID 0: %q", name)
			}
			// Roundtrip should match or produce valid format
			formatted := wal.SegmentName(id)
			roundtripID, rtErr := wal.ParseSegmentID(formatted)
			if rtErr != nil || roundtripID != id {
				t.Fatalf("roundtrip mismatch for %q -> id %d -> formatted %q -> rtID %d (err: %v)", name, id, formatted, roundtripID, rtErr)
			}
		}
	})
}
