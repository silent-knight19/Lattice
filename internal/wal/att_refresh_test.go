package wal_test

import (
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/wal"
)

// The refreshed attestation must describe the segment accurately, including
// lastSeqNum, which used to be hardcoded to 0.
func TestAttestation_RefreshRecordsLastSeqNum(t *testing.T) {
	dbPath := t.TempDir()
	buildSegments(t, dbPath, 6, 0)

	ids, _ := wal.ListSegments(dbPath)
	active := ids[len(ids)-1]

	if _, err := wal.RecoverWAL(dbPath, nil); err != nil {
		t.Fatalf("RecoverWAL: %v", err)
	}
	att, err := wal.ReadAttestation(dbPath, active)
	if err != nil {
		t.Fatalf("ReadAttestation: %v", err)
	}
	t.Logf("attestation after recovery: %+v", att)
	if att.LastSeqNum != binary.SeqNum(6) {
		t.Errorf("LastSeqNum = %d; want 6 (recovery refresh hardcoded 0 previously)", att.LastSeqNum)
	}
	if att.RecordCount != 6 {
		t.Errorf("RecordCount = %d; want 6", att.RecordCount)
	}
	if att.Sealed {
		t.Error("active segment must be attested unsealed")
	}
}
