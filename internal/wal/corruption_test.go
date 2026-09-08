package wal_test

import (
	"bytes"
	stdErrors "errors"
	"io"
	"math/rand"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/wal"
)

// TestCRCFieldCorruption verifies that mutating each byte of the CRC32 checksum field
// (byte offsets 0, 1, 2, and 3) is strictly detected as an ErrChecksumMismatch.
//
// Key Architectural Invariant:
// The 4-byte CRC field itself is explicitly excluded from the checksum calculation input.
// Therefore, changing CRC bytes directly alters the expected checksum without modifying the
// computed checksum of the payload, guaranteeing a ChecksumMismatchError.
func TestCRCFieldCorruption(t *testing.T) {
	rec := wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    binary.SeqNum(42),
		Timestamp: 1700000000000,
		Key:       []byte("crc-test-key"),
		Value:     []byte("crc-test-value-payload"),
	}

	validBytes, err := wal.EncodeRecord(rec)
	if err != nil {
		t.Fatalf("EncodeRecord failed: %v", err)
	}

	// Verify baseline decode succeeds
	decoded, err := wal.DecodeRecord(bytes.NewReader(validBytes))
	if err != nil {
		t.Fatalf("baseline DecodeRecord failed: %v", err)
	}
	if !decoded.Equal(rec) {
		t.Fatalf("decoded record mismatch: got %+v, want %+v", decoded, rec)
	}

	storedCRC := binary.GetUint32(validBytes[0:4])
	if storedCRC == 0 {
		t.Fatalf("stored CRC should not be 0 for non-empty record")
	}

	// Test each of the 4 CRC bytes independently across all 8 bit positions
	for byteOffset := 0; byteOffset < 4; byteOffset++ {
		for bit := 0; bit < 8; bit++ {
			corrupted := make([]byte, len(validBytes))
			copy(corrupted, validBytes)
			corrupted[byteOffset] ^= (1 << bit)

			corruptedStoredCRC := binary.GetUint32(corrupted[0:4])
			if corruptedStoredCRC == storedCRC {
				t.Fatalf("bit flip at byte %d bit %d did not alter stored CRC", byteOffset, bit)
			}

			_, err := wal.DecodeRecord(bytes.NewReader(corrupted))
			if err == nil {
				t.Fatalf("corruption in CRC byte %d (bit %d) was silently accepted", byteOffset, bit)
			}

			// Must match ErrChecksumMismatch sentinel
			if !stdErrors.Is(err, errors.ErrChecksumMismatch) {
				t.Fatalf("expected ErrChecksumMismatch at CRC byte %d bit %d, got: %v", byteOffset, bit, err)
			}

			// Must carry structured diagnostic context
			var chkErr *errors.ChecksumMismatchError
			if !stdErrors.As(err, &chkErr) {
				t.Fatalf("expected *errors.ChecksumMismatchError, got %T: %v", err, err)
			}

			if chkErr.Expected != corruptedStoredCRC {
				t.Fatalf("ChecksumMismatchError expected CRC mismatch: got 0x%08x, want 0x%08x",
					chkErr.Expected, corruptedStoredCRC)
			}
			if chkErr.Actual != storedCRC {
				t.Fatalf("ChecksumMismatchError actual CRC mismatch: got 0x%08x, want 0x%08x",
					chkErr.Actual, storedCRC)
			}
		}
	}
}

// TestCRCScopeCoverage explicitly proves that the CRC32-IEEE checksum covers
// every single post-CRC byte in the physical record:
//   - RecordType (offset 4)
//   - SeqNum (offsets 5..12)
//   - Timestamp (offsets 13..20)
//   - KeyLength (offsets 21..22)
//   - KeyBytes (offsets 23..23+KeyLen-1)
//   - ValueLength (offsets 23+KeyLen..26+KeyLen)
//   - ValueBytes (offsets 27+KeyLen..end)
//
// And confirms that the 4-byte CRC field itself is strictly EXCLUDED from the calculation.
func TestCRCScopeCoverage(t *testing.T) {
	rec := wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    binary.SeqNum(9876543210),
		Timestamp: 1698765432100,
		Key:       []byte("scope-key"),
		Value:     []byte("scope-value-bytes"),
	}

	encoded, err := wal.EncodeRecord(rec)
	if err != nil {
		t.Fatalf("EncodeRecord failed: %v", err)
	}

	storedCRC := binary.GetUint32(encoded[0:4])

	// 1. Independently compute CRC over all bytes after offset 4 using binary.Checksum
	independentCRC := binary.Checksum(encoded[4:])
	if storedCRC != independentCRC {
		t.Fatalf("stored CRC (0x%08x) != independent CRC over encoded[4:] (0x%08x)",
			storedCRC, independentCRC)
	}

	// 2. Prove that changing CRC field bytes (offsets 0..3) does NOT change independentCRC
	for i := 0; i < 4; i++ {
		tamperedHeader := make([]byte, len(encoded))
		copy(tamperedHeader, encoded)
		tamperedHeader[i] ^= 0xFF

		recomputed := binary.Checksum(tamperedHeader[4:])
		if recomputed != independentCRC {
			t.Fatalf("mutating CRC field byte %d altered payload checksum! CRC field must be excluded", i)
		}
	}

	// 3. Systematically prove that mutating ANY field in the post-CRC physical layout
	// alters the computed checksum:
	fields := []struct {
		name       string
		startOff   int
		endOff     int
		mutateMask byte
	}{
		{name: "RecordType", startOff: 4, endOff: 4, mutateMask: 0x03},
		{name: "SeqNum", startOff: 5, endOff: 12, mutateMask: 0x01},
		{name: "Timestamp", startOff: 13, endOff: 20, mutateMask: 0x01},
		{name: "KeyLength", startOff: 21, endOff: 22, mutateMask: 0x01},
		{name: "KeyBytes", startOff: 23, endOff: 23 + len(rec.Key) - 1, mutateMask: 0x20},
		{name: "ValueLength", startOff: 23 + len(rec.Key), endOff: 26 + len(rec.Key), mutateMask: 0x01},
		{name: "ValueBytes", startOff: 27 + len(rec.Key), endOff: len(encoded) - 1, mutateMask: 0x40},
	}

	for _, f := range fields {
		for off := f.startOff; off <= f.endOff; off++ {
			mutated := make([]byte, len(encoded))
			copy(mutated, encoded)
			mutated[off] ^= f.mutateMask

			mutatedCRC := binary.Checksum(mutated[4:])
			if mutatedCRC == independentCRC {
				t.Fatalf("field %s at offset %d: checksum collision! Mutated checksum 0x%08x == original 0x%08x",
					f.name, off, mutatedCRC, independentCRC)
			}
		}
	}
}

// TestExhaustiveSingleBitCorruption performs a deterministic, exhaustive single-bit flip test
// across all checksum-covered bytes (offsets 4 through len-1).
//
// For every byte offset and for each of the 8 bits:
//  1. Mutate bit b in byte i
//  2. Verify DecodeRecord fails (detecting corruption via checksum mismatch or structural error)
//  3. Restore bit b in byte i
//  4. Verify DecodeRecord succeeds cleanly and returns the original valid record.
func TestExhaustiveSingleBitCorruption(t *testing.T) {
	rec := wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    binary.SeqNum(88888),
		Timestamp: 1712345678000,
		Key:       []byte("exhaustive-key"),
		Value:     []byte("exhaustive-payload-val"),
	}

	validBytes, err := wal.EncodeRecord(rec)
	if err != nil {
		t.Fatalf("EncodeRecord failed: %v", err)
	}

	totalBytes := len(validBytes)
	coveredBytes := totalBytes - 4 // offsets 4 to totalBytes-1
	expectedFlips := coveredBytes * 8

	corrupted := make([]byte, totalBytes)
	copy(corrupted, validBytes)

	checksumMismatchCount := 0
	structuralErrorCount := 0
	totalTested := 0

	for offset := 4; offset < totalBytes; offset++ {
		for bit := 0; bit < 8; bit++ {
			totalTested++
			mask := byte(1 << bit)

			// Step 1: Invert bit
			corrupted[offset] ^= mask

			// Step 2: Decode and verify corruption is detected
			_, decodeErr := wal.DecodeRecord(bytes.NewReader(corrupted))
			if decodeErr == nil {
				t.Fatalf("bit flip at offset %d bit %d was silently accepted!", offset, bit)
			}

			// Step 3: Classify error into Checksum Mismatch vs Structural Validation
			if stdErrors.Is(decodeErr, errors.ErrChecksumMismatch) {
				checksumMismatchCount++
			} else if isRecognizedStructuralError(decodeErr) {
				structuralErrorCount++
			} else {
				t.Fatalf("offset %d bit %d produced unrecognized error: %v", offset, bit, decodeErr)
			}

			// Step 4: Restore bit
			corrupted[offset] ^= mask

			// Step 5: Verify original valid state is restored
			restoredRec, restoreErr := wal.DecodeRecord(bytes.NewReader(corrupted))
			if restoreErr != nil {
				t.Fatalf("failed to decode restored record at offset %d bit %d: %v", offset, bit, restoreErr)
			}
			if !restoredRec.Equal(rec) {
				t.Fatalf("restored record differs from original at offset %d bit %d", offset, bit)
			}
		}
	}

	if totalTested != expectedFlips {
		t.Fatalf("expected %d bit flips tested, got %d", expectedFlips, totalTested)
	}

	t.Logf("Exhaustive bit-flip results: %d tested, %d checksum mismatches, %d structural errors, 0 silent accepts",
		totalTested, checksumMismatchCount, structuralErrorCount)

	if checksumMismatchCount == 0 {
		t.Fatalf("expected non-zero checksum mismatches, got 0")
	}
}

// isRecognizedStructuralError checks whether an error is a legitimate structural/framing rejection
// that intercepts corruption before checksum calculation.
func isRecognizedStructuralError(err error) bool {
	return stdErrors.Is(err, errors.ErrInvalidRecordType) ||
		stdErrors.Is(err, errors.ErrInvalidRecordPayload) ||
		stdErrors.Is(err, errors.ErrKeyTooLarge) ||
		stdErrors.Is(err, errors.ErrValueTooLarge) ||
		stdErrors.Is(err, errors.ErrEmptyKey) ||
		stdErrors.Is(err, errors.ErrHeaderTruncated) ||
		stdErrors.Is(err, io.ErrUnexpectedEOF)
}

// TestStructuralVsChecksumRejectionMatrix systematically verifies the behavioral distinction
// between:
//   - STRUCTURAL REJECTION: Early format validation that aborts before checksum calculation
//     to prevent unbounded resource consumption or invalid state.
//   - CHECKSUM REJECTION: Framing is syntactically valid within bounds, but payload has suffered
//     data corruption, detected strictly by CRC32-IEEE verification.
func TestStructuralVsChecksumRejectionMatrix(t *testing.T) {
	basePut := wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    binary.SeqNum(1001),
		Timestamp: 1700000000001,
		Key:       []byte("matrix-key"),
		Value:     []byte("matrix-val"),
	}
	putBytes, err := wal.EncodeRecord(basePut)
	if err != nil {
		t.Fatalf("EncodeRecord basePut failed: %v", err)
	}

	baseBatch := wal.Record{
		Type:      wal.RecordTypeBatchStart,
		SeqNum:    binary.SeqNum(1002),
		Timestamp: 1700000000002,
	}
	batchBytes, err := wal.EncodeRecord(baseBatch)
	if err != nil {
		t.Fatalf("EncodeRecord baseBatch failed: %v", err)
	}

	tests := []struct {
		name              string
		base              []byte
		mutateFn          func(buf []byte)
		expectedErrTarget error
		expectChecksumErr bool
		description       string
	}{
		{
			name: "TypeByteCorruptedToZero",
			base: putBytes,
			mutateFn: func(b []byte) {
				b[4] = 0x00 // RecordTypeInvalid
			},
			expectedErrTarget: errors.ErrInvalidRecordType,
			expectChecksumErr: false,
			description:       "RecordType(0x00) must be structurally rejected as uninitialized/invalid before checksum",
		},
		{
			name: "TypeByteCorruptedToUnassignedEnum",
			base: putBytes,
			mutateFn: func(b []byte) {
				b[4] = 0x05 // Beyond RecordTypeBatchCommit (0x04)
			},
			expectedErrTarget: errors.ErrInvalidRecordType,
			expectChecksumErr: false,
			description:       "Unrecognized RecordType enum must be structurally rejected before checksum",
		},
		{
			name: "ValueLengthExceedsArchitecturalLimit",
			base: putBytes,
			mutateFn: func(b []byte) {
				// PUT key length is 10 bytes ("matrix-key").
				// ValueLength offset is 23 + 10 = 33.
				valLenOff := 23 + len(basePut.Key)
				binary.PutUint32(b[valLenOff:valLenOff+4], 0xFFFFFFFF) // 4 GiB
			},
			expectedErrTarget: errors.ErrValueTooLarge,
			expectChecksumErr: false,
			description:       "Anti-DoS: Oversized ValueLength (> 4 MiB) must be rejected before memory allocation or checksumming",
		},
		{
			name: "BatchMarkerCorruptedToHaveNonEmptyKey",
			base: batchBytes,
			mutateFn: func(b []byte) {
				// Offset 21..22 is KeyLength. Mutate to 2.
				binary.PutUint16(b[21:23], 2)
			},
			expectedErrTarget: errors.ErrInvalidRecordPayload,
			expectChecksumErr: false,
			description:       "BATCH_START with KeyLength > 0 violates type constraints and must be rejected structurally",
		},
		{
			name: "SeqNumCorrupted",
			base: putBytes,
			mutateFn: func(b []byte) {
				b[5] ^= 0x01 // Mutate highest byte of SeqNum
			},
			expectedErrTarget: errors.ErrChecksumMismatch,
			expectChecksumErr: true,
			description:       "SeqNum corruption preserves valid framing but must fail CRC32 checksum verification",
		},
		{
			name: "TimestampCorrupted",
			base: putBytes,
			mutateFn: func(b []byte) {
				b[13] ^= 0x01 // Mutate highest byte of Timestamp
			},
			expectedErrTarget: errors.ErrChecksumMismatch,
			expectChecksumErr: true,
			description:       "Timestamp corruption preserves valid framing but must fail CRC32 checksum verification",
		},
		{
			name: "KeyPayloadCorrupted",
			base: putBytes,
			mutateFn: func(b []byte) {
				b[23] ^= 0xAA // Mutate first byte of user key
			},
			expectedErrTarget: errors.ErrChecksumMismatch,
			expectChecksumErr: true,
			description:       "Key byte corruption preserves valid framing but must fail CRC32 checksum verification",
		},
		{
			name: "ValuePayloadCorrupted",
			base: putBytes,
			mutateFn: func(b []byte) {
				b[len(b)-1] ^= 0x55 // Mutate last byte of value
			},
			expectedErrTarget: errors.ErrChecksumMismatch,
			expectChecksumErr: true,
			description:       "Value byte corruption preserves valid framing but must fail CRC32 checksum verification",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			corrupted := make([]byte, len(tc.base))
			copy(corrupted, tc.base)
			tc.mutateFn(corrupted)

			_, err := wal.DecodeRecord(bytes.NewReader(corrupted))
			if err == nil {
				t.Fatalf("[%s] expected error, got nil", tc.name)
			}

			if !stdErrors.Is(err, tc.expectedErrTarget) {
				t.Fatalf("[%s] error mismatch: got %v, want matching target %v (%s)",
					tc.name, err, tc.expectedErrTarget, tc.description)
			}

			if tc.expectChecksumErr {
				var chkErr *errors.ChecksumMismatchError
				if !stdErrors.As(err, &chkErr) {
					t.Fatalf("[%s] expected *errors.ChecksumMismatchError, got %T: %v", tc.name, err, err)
				}
			}
		})
	}
}

// TestCorruptionSemantics_ReversibilityAndCollisions verifies the 5 core corruption semantics:
//   - A. Mutation to a valid record is detected.
//   - B. Original valid record still decodes successfully.
//   - C. Mutation followed by restoration returns original valid behavior.
//   - D. Different corruptions do not collide into another valid record.
//   - E. Checksum verification is strictly deterministic.
func TestCorruptionSemantics_ReversibilityAndCollisions(t *testing.T) {
	rec := wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    binary.SeqNum(55555),
		Timestamp: 1700005555500,
		Key:       []byte("semantics-test-key"),
		Value:     []byte("semantics-test-value"),
	}

	encoded, err := wal.EncodeRecord(rec)
	if err != nil {
		t.Fatalf("EncodeRecord failed: %v", err)
	}

	// Semantics B: Original decodes cleanly
	origDecoded, err := wal.DecodeRecord(bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("Semantic B failed: original record failed to decode: %v", err)
	}
	if !origDecoded.Equal(rec) {
		t.Fatalf("Semantic B failed: decoded record mismatch")
	}

	// Semantics A & C: Mutation detected, restoration restores validity
	mutated := make([]byte, len(encoded))
	copy(mutated, encoded)
	mutated[10] ^= 0x7F // mutate SeqNum byte

	_, err = wal.DecodeRecord(bytes.NewReader(mutated))
	if err == nil {
		t.Fatalf("Semantic A failed: mutation was silently accepted")
	}
	if !stdErrors.Is(err, errors.ErrChecksumMismatch) {
		t.Fatalf("Semantic A failed: expected ErrChecksumMismatch, got %v", err)
	}

	// Restore mutation
	mutated[10] ^= 0x7F
	restored, err := wal.DecodeRecord(bytes.NewReader(mutated))
	if err != nil {
		t.Fatalf("Semantic C failed: restoration failed to decode: %v", err)
	}
	if !restored.Equal(rec) {
		t.Fatalf("Semantic C failed: restored record does not match original")
	}

	// Semantics D: Multiple distinct corruptions do not collide into a valid record
	for i := 0; i < 50; i++ {
		tampered := make([]byte, len(encoded))
		copy(tampered, encoded)

		// Introduce byte modification across payload with strictly non-zero mask
		mask := byte((i % 255) + 1)
		offset := 4 + (i % (len(tampered) - 4))
		tampered[offset] ^= mask

		_, err := wal.DecodeRecord(bytes.NewReader(tampered))
		if err == nil {
			t.Fatalf("Semantic D failed: random mutation %d produced an accidental valid record!", i)
		}
	}

	// Semantics E: Deterministic verification
	var expectedChk uint32
	var actualChk uint32
	corruptedBuf := make([]byte, len(encoded))
	copy(corruptedBuf, encoded)
	corruptedBuf[15] ^= 0xEE // Corrupt Timestamp

	for run := 0; run < 100; run++ {
		_, err := wal.DecodeRecord(bytes.NewReader(corruptedBuf))
		if err == nil {
			t.Fatalf("Semantic E failed: run %d did not fail", run)
		}
		var chkErr *errors.ChecksumMismatchError
		if !stdErrors.As(err, &chkErr) {
			t.Fatalf("Semantic E failed: run %d did not return *ChecksumMismatchError: %v", run, err)
		}

		if run == 0 {
			expectedChk = chkErr.Expected
			actualChk = chkErr.Actual
		} else {
			if chkErr.Expected != expectedChk || chkErr.Actual != actualChk {
				t.Fatalf("Semantic E failed: non-deterministic checksum error at run %d: got (%x, %x), want (%x, %x)",
					run, chkErr.Expected, chkErr.Actual, expectedChk, actualChk)
			}
		}
	}
}

// TestDeterministicRandomizedCorruption executes a seeded randomized corruption suite
// generating records across all 4 record types (PUT, DELETE, BATCH_START, BATCH_COMMIT)
// with varied key lengths and value lengths.
//
// Every record is encoded, exactly one randomly selected byte is mutated, and the decoder
// is asserted to intercept corruption in 100% of cases.
func TestDeterministicRandomizedCorruption(t *testing.T) {
	// Fixed deterministic seed guarantees reproducible test runs across all platforms and CI
	rng := rand.New(rand.NewSource(1337))

	recordTypes := []wal.RecordType{
		wal.RecordTypePut,
		wal.RecordTypeDelete,
		wal.RecordTypeBatchStart,
		wal.RecordTypeBatchCommit,
	}

	const iterations = 1000
	interceptedCount := 0

	for i := 0; i < iterations; i++ {
		recType := recordTypes[rng.Intn(len(recordTypes))]
		var key []byte
		var val []byte

		switch recType {
		case wal.RecordTypePut:
			keyLen := rng.Intn(128) + 1 // 1..128 bytes
			valLen := rng.Intn(512)     // 0..511 bytes
			key = make([]byte, keyLen)
			val = make([]byte, valLen)
			rng.Read(key)
			rng.Read(val)
		case wal.RecordTypeDelete:
			keyLen := rng.Intn(128) + 1 // 1..128 bytes
			key = make([]byte, keyLen)
			rng.Read(key)
			// val must be nil/empty
		case wal.RecordTypeBatchStart, wal.RecordTypeBatchCommit:
			// key and val must both be nil/empty
		}

		rec := wal.Record{
			Type:      recType,
			SeqNum:    binary.SeqNum(rng.Uint64()),
			Timestamp: rng.Uint64(),
			Key:       key,
			Value:     val,
		}

		encoded, err := wal.EncodeRecord(rec)
		if err != nil {
			t.Fatalf("iteration %d: EncodeRecord failed: %v", i, err)
		}

		// Mutate exactly one byte anywhere in the record [0..len-1]
		corruptOffset := rng.Intn(len(encoded))
		corruptMask := byte(rng.Intn(255) + 1) // 1..255 (guaranteed non-zero mutation)

		corrupted := make([]byte, len(encoded))
		copy(corrupted, encoded)
		corrupted[corruptOffset] ^= corruptMask

		_, decodeErr := wal.DecodeRecord(bytes.NewReader(corrupted))
		if decodeErr == nil {
			t.Fatalf("iteration %d: corruption at offset %d (type %s, len %d) was silently accepted!",
				i, corruptOffset, recType.String(), len(encoded))
		}

		// Verify error is an expected domain/checksum failure
		isExpected := stdErrors.Is(decodeErr, errors.ErrChecksumMismatch) ||
			isRecognizedStructuralError(decodeErr)

		if !isExpected {
			t.Fatalf("iteration %d: unexpected error type %T: %v", i, decodeErr, decodeErr)
		}

		interceptedCount++
	}

	if interceptedCount != iterations {
		t.Fatalf("expected %d corruptions intercepted, got %d", iterations, interceptedCount)
	}

	t.Logf("Deterministic randomized corruption: 100%% intercepted (%d/%d records across 4 types)",
		interceptedCount, iterations)
}

// TestCRCCollisionCaveat_TheoreticalLimits documents and verifies the operational boundaries
// of CRC32-IEEE checksum verification:
//
// Theoretical Properties & Limitations:
//  1. Error Detection vs Cryptography:
//     CRC32-IEEE is a cyclic redundancy check designed to detect random bit-rot, torn blocks,
//     and transmission errors. It is NOT a cryptographic MAC or digital signature. An attacker
//     with write access can compute a valid CRC32 for any arbitrary payload.
//  2. Collision Probability:
//     The checksum has a 32-bit state space (2^32 ≈ 4.29 billion values). On random corruptions
//     that flip multiple bits simultaneously, the theoretical probability of an undetected error
//     is 2^-32 ≈ 2.33 x 10^-10.
//  3. Single-Bit Guarantees:
//     CRC32-IEEE provides a Hamming distance guarantee: 100% of single-bit errors are
//     mathematically guaranteed to be detected for block sizes up to the polynomial's cycle limit.
func TestCRCCollisionCaveat_TheoreticalLimits(t *testing.T) {
	// Construct valid record
	rec := wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    binary.SeqNum(1),
		Timestamp: 100,
		Key:       []byte("caveat-key"),
		Value:     []byte("caveat-val"),
	}

	encoded, err := wal.EncodeRecord(rec)
	if err != nil {
		t.Fatalf("EncodeRecord failed: %v", err)
	}

	originalCRC := binary.GetUint32(encoded[0:4])

	// Verify that deliberately creating a crafted payload with its own valid CRC32
	// decodes successfully — demonstrating that CRC32 does NOT provide cryptographic authentication.
	crafted := make([]byte, len(encoded))
	copy(crafted, encoded)
	crafted[len(crafted)-1] ^= 0xFF // Alter payload
	craftedCRC := binary.Checksum(crafted[4:])
	binary.PutUint32(crafted[0:4], craftedCRC) // Forger updates CRC to match

	tamperedRecord, err := wal.DecodeRecord(bytes.NewReader(crafted))
	if err != nil {
		t.Fatalf("expected crafted record with recomputed CRC to decode (demonstrating non-cryptographic nature): %v", err)
	}

	if tamperedRecord.CRC == originalCRC {
		t.Fatalf("tampered CRC should differ from original CRC")
	}

	// This proves that CRC32 detects accidental data corruption, but cannot prevent intentional tampering.
	t.Logf("CRC Caveat verified: Accidental corruption is detected; intentional re-computation succeeds (non-cryptographic).")
}
