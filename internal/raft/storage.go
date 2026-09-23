package raft

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/cluster"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/transport"
)

const (
	// StateFilename is the canonical name of the active Raft metadata state file.
	StateFilename = "raft_state"

	// StateTempFilename is the staging filename used for atomic metadata updates.
	StateTempFilename = "raft_state.tmp"

	// LogFilename is the canonical name of the append-only Raft log segment file.
	LogFilename = "raft.log"

	// LogTempFilename is the staging filename used when creating or truncating the log file.
	LogTempFilename = "raft.log.tmp"

	// RaftFileMode defines the restrictive file permissions (0600) for Raft state and log files:
	// owner: read + write; group: none; others: none.
	RaftFileMode os.FileMode = 0600

	// StateMagic is the 4-byte header magic identifying valid Raft metadata files ("RFST" = 0x52465354).
	StateMagic uint32 = 0x52465354

	// StateVersion defines the current binary format version of the Raft state file.
	StateVersion uint16 = 1

	// StatePayloadSize is the fixed size in bytes of the Raft metadata state payload:
	// Magic (4B) | Version (2B) | Reserved (2B) | Term (8B) | VotedFor (8B) = 24 bytes.
	StatePayloadSize = 24

	// StateRecordSize is the total size of the state file including the trailing CRC32-IEEE checksum:
	// StatePayloadSize (24B) + CRC32 (4B) = 28 bytes.
	StateRecordSize = StatePayloadSize + 4

	// LogRecordHeaderSize is the fixed size in bytes of each persistent Raft log record framing header:
	// CRC32 (4B) | Index (8B) | Term (8B) | Type (1B) | DataLen (4B) = 25 bytes.
	LogRecordHeaderSize = 25
)

// Pluggable seams for fault-injection testing
var (
	raftSyncDirFn   = syncDir
	raftRenameFn    = os.Rename
	raftOpenFileFn  = os.OpenFile
	raftCreateTmpFn = func(path string, flag int, perm os.FileMode) (*os.File, error) {
		return os.OpenFile(path, flag, perm)
	}
	raftWriteTmpFn = func(f *os.File, b []byte) (int, error) {
		return f.Write(b)
	}
	raftSyncTmpFn = func(f *os.File) error {
		return fdatasync(f)
	}
	raftCloseTmpFn = func(f *os.File) error {
		return f.Close()
	}
)

// syncDir flushes modified directory entries to persistent storage media.
func syncDir(dirPath string) error {
	df, err := os.Open(dirPath)
	if err != nil {
		return err
	}
	defer func() { _ = df.Close() }()

	if err := df.Sync(); err != nil && runtime.GOOS != "windows" {
		return err
	}
	return nil
}

// Storage manages persistent Raft state (`currentTerm`, `votedFor`) and log entries (`log[]`).
//
// Durability & Consistency Contract:
//   - Metadata (`HardState`) is persisted via atomic file replacement:
//     written to `raft_state.tmp`, fsynced via fdatasync, atomically renamed to `raft_state`,
//     and followed by parent directory fsync.
//   - Log entries are appended sequentially with CRC32 framing and fsynced.
//   - On restart / recovery:
//     1. Metadata is loaded, CRC32 validated, and version checked.
//     2. Log records are iterated, CRC32 validated, and contiguous indices verified.
//     3. If an uncommitted partial record exists at the physical end of the log (EOF),
//     it is truncated cleanly.
//     4. Mid-log bit flips or corrupted records fail closed with ErrChecksumMismatch.
//     5. Term regressions are strictly rejected.
type Storage struct {
	mu        sync.RWMutex
	dir       string
	closed    atomic.Bool
	hardState HardState
	memLog    *InMemLog
	logFile   *os.File // Open append-only file descriptor pinned to disk inode
}

// StorageOptions configures the Raft persistent storage.
type StorageOptions struct {
	Dir string
}

// OpenStorage initializes or recovers Raft persistent state and log from dir.
//
// Invariants enforced:
//   - dir must be non-empty and must exist as a genuine directory (not a symlink).
//   - If state file does not exist, initializes term 0, votedFor 0, and an empty log.
//   - Recovers durable term, votedFor, and log entries.
//   - Pins opened log file descriptor with os.O_APPEND.
func OpenStorage(dir string) (*Storage, error) {
	if dir == "" {
		return nil, fmt.Errorf("%w: storage directory path cannot be empty", os.ErrInvalid)
	}

	cleanDir := filepath.Clean(dir)
	dirInfo, err := os.Lstat(cleanDir)
	if err != nil {
		return nil, fmt.Errorf("raft: failed to inspect storage directory %s: %w", cleanDir, err)
	}
	if dirInfo.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: storage directory %s cannot be a symbolic link", os.ErrInvalid, cleanDir)
	}
	if !dirInfo.IsDir() {
		return nil, &errors.NotADirectoryError{Path: cleanDir, Mode: dirInfo.Mode()}
	}

	// 1. Recover or initialize HardState
	statePath := filepath.Join(cleanDir, StateFilename)
	hs, err := readStateFile(statePath)
	if err != nil {
		if os.IsNotExist(err) {
			// Cold-boot zero state: persist initial state file atomically
			hs = HardState{Term: 0, VotedFor: cluster.NodeIDNil}
			if err := writeStateFile(cleanDir, hs); err != nil {
				return nil, err
			}
		} else {
			return nil, fmt.Errorf("raft: failed to recover hard state: %w", err)
		}
	}

	// 2. Recover log entries
	logPath := filepath.Join(cleanDir, LogFilename)
	entries, err := recoverLogFile(logPath)
	if err != nil {
		return nil, fmt.Errorf("raft: failed to recover raft log: %w", err)
	}

	for _, e := range entries {
		if e.Term > hs.Term {
			return nil, fmt.Errorf("%w: recovered entry %d term %d exceeds current term %d",
				errors.ErrRaftCorruptedState, e.Index, e.Term, hs.Term)
		}
	}

	inMemLog, err := NewInMemLogWithEntries(entries)
	if err != nil {
		return nil, fmt.Errorf("raft: log contiguity validation failed: %w", err)
	}

	// 3. Open log file for appending with restrictive 0600 permissions
	_, statErr := os.Stat(logPath)
	isNew := os.IsNotExist(statErr)

	logFile, err := raftOpenFileFn(logPath, os.O_CREATE|os.O_RDWR|os.O_APPEND, RaftFileMode)
	if err != nil {
		return nil, fmt.Errorf("raft: failed to open log file %s: %w", logPath, err)
	}

	if isNew {
		if err := raftSyncDirFn(cleanDir); err != nil {
			_ = logFile.Close()
			return nil, fmt.Errorf("raft: failed to sync directory on initial log creation: %w", err)
		}
	}

	// Pin open descriptor against symlink swap
	fi, err := logFile.Stat()
	if err != nil {
		_ = logFile.Close()
		return nil, err
	}
	lstatFi, err := os.Lstat(logPath)
	if err != nil {
		_ = logFile.Close()
		return nil, err
	}
	if !os.SameFile(fi, lstatFi) {
		_ = logFile.Close()
		return nil, fmt.Errorf("%w: raft log file descriptor swapped during open", os.ErrInvalid)
	}

	s := &Storage{
		dir:       cleanDir,
		hardState: hs,
		memLog:    inMemLog,
		logFile:   logFile,
	}

	return s, nil
}

// HardState returns a copy of the current durable metadata (term, votedFor).
func (s *Storage) HardState() (HardState, error) {
	if s.closed.Load() {
		return HardState{}, errors.ErrRaftStateClosed
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.hardState.Clone(), nil
}

// Term returns the current election term.
func (s *Storage) Term() (Term, error) {
	if s.closed.Load() {
		return 0, errors.ErrRaftStateClosed
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.hardState.Term, nil
}

// VotedFor returns the candidate node ID voted for in the current term, or NodeIDNil (0).
func (s *Storage) VotedFor() (cluster.NodeID, error) {
	if s.closed.Load() {
		return cluster.NodeIDNil, errors.ErrRaftStateClosed
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.hardState.VotedFor, nil
}

// SetHardState atomically and durably persists new metadata (term, votedFor).
//
// Invariants enforced:
//   - Term must be >= currentTerm (monotonicity; regression strictly rejected).
//   - If term > currentTerm, votedFor can be any valid candidate or NodeIDNil.
//   - If term == currentTerm, cannot change an existing vote to a different candidate.
//   - Metadata is atomically synced and replaced on disk before returning nil.
func (s *Storage) SetHardState(hs HardState) error {
	if s.closed.Load() {
		return errors.ErrRaftStateClosed
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed.Load() {
		return errors.ErrRaftStateClosed
	}

	// 0. Structural validation
	if err := hs.Validate(); err != nil {
		return err
	}

	// 1. Monotonicity check
	if hs.Term < s.hardState.Term {
		return fmt.Errorf("%w: attempted term %d < current term %d",
			errors.ErrRaftTermRegressed, hs.Term, s.hardState.Term)
	}

	// 2. Same-term vote rules:
	//    - Cannot vote for a different node in the same term (vote uniqueness).
	//    - Cannot clear or reset an already cast vote in the same term.
	if hs.Term == s.hardState.Term {
		if s.hardState.VotedFor != cluster.NodeIDNil {
			if hs.VotedFor == cluster.NodeIDNil {
				return fmt.Errorf("%w: cannot clear vote for node %d in term %d",
					errors.ErrRaftVoteClearedInSameTerm, s.hardState.VotedFor, hs.Term)
			}
			if hs.VotedFor != s.hardState.VotedFor {
				return fmt.Errorf("%w: already voted for node %d in term %d, cannot vote for %d",
					errors.ErrRaftDuplicateVote, s.hardState.VotedFor, hs.Term, hs.VotedFor)
			}
		}
	}

	// 3. Durably persist to disk via atomic file replacement
	if err := writeStateFile(s.dir, hs); err != nil {
		return fmt.Errorf("raft: failed to persist hard state: %w", err)
	}

	s.hardState = hs.Clone()
	return nil
}

// SetTerm advances the current term and clears votedFor if newTerm > currentTerm.
// If newTerm == currentTerm, it is a no-op and preserves any existing vote.
// Returns ErrRaftTermRegressed if newTerm < currentTerm.
func (s *Storage) SetTerm(newTerm Term) error {
	s.mu.RLock()
	currTerm := s.hardState.Term
	s.mu.RUnlock()

	if newTerm < currTerm {
		return fmt.Errorf("%w: attempted term %d < current term %d",
			errors.ErrRaftTermRegressed, newTerm, currTerm)
	}
	if newTerm == currTerm {
		// Same term: preserve existing vote and avoid unnecessary disk write
		return nil
	}

	return s.SetHardState(HardState{
		Term:     newTerm,
		VotedFor: cluster.NodeIDNil,
	})
}

// SetVote records a vote for candidateID in the current term.
// Returns ErrRaftDuplicateVote if already voted for a different candidate in this term.
func (s *Storage) SetVote(candidateID cluster.NodeID) error {
	s.mu.RLock()
	currTerm := s.hardState.Term
	s.mu.RUnlock()

	return s.SetHardState(HardState{
		Term:     currTerm,
		VotedFor: candidateID,
	})
}

// LastIndexAndTerm returns the index and term of the last entry in the log.
func (s *Storage) LastIndexAndTerm() (LogIndex, Term, error) {
	if s.closed.Load() {
		return 0, 0, errors.ErrRaftStateClosed
	}
	idx, term := s.memLog.LastIndexAndTerm()
	return idx, term, nil
}

// LastIndex returns the index of the last entry in the log.
func (s *Storage) LastIndex() (LogIndex, error) {
	if s.closed.Load() {
		return 0, errors.ErrRaftStateClosed
	}
	return s.memLog.LastIndex(), nil
}

// TermOf returns the term of the entry at logical index.
func (s *Storage) TermOf(index LogIndex) (Term, error) {
	if s.closed.Load() {
		return 0, errors.ErrRaftStateClosed
	}
	return s.memLog.Term(index)
}

// Entry returns a deep copy of the entry at logical index.
func (s *Storage) Entry(index LogIndex) (LogEntry, error) {
	if s.closed.Load() {
		return LogEntry{}, errors.ErrRaftStateClosed
	}
	return s.memLog.Entry(index)
}

// Entries returns deep copies of entries in the half-open range [from, to).
func (s *Storage) Entries(from, to LogIndex) ([]LogEntry, error) {
	if s.closed.Load() {
		return nil, errors.ErrRaftStateClosed
	}
	return s.memLog.Entries(from, to)
}

// Append appends entries durably to the persistent Raft log.
//
// Invariants enforced:
//   - If entries is empty, returns nil without I/O.
//   - Entries must have contiguous 1-based indices matching LastIndex() + 1.
//   - Each entry is encoded with CRC32-IEEE framing header.
//   - Flushed to disk with fdatasync before mutating in-memory log.
//   - On write or sync error, in-memory log remains completely untouched (all-or-nothing).
func (s *Storage) Append(entries ...LogEntry) error {
	if len(entries) == 0 {
		return nil
	}
	if s.closed.Load() {
		return errors.ErrRaftStateClosed
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed.Load() {
		return errors.ErrRaftStateClosed
	}

	if s.logFile == nil {
		return fmt.Errorf("%w: log file descriptor is nil", errors.ErrRaftStateClosed)
	}

	lastIdx := s.memLog.LastIndex()
	currTerm := s.hardState.Term

	// 1. Validate term relationship, contiguity, and payload bounds before any serialization
	for _, e := range entries {
		if e.Term > currTerm {
			return fmt.Errorf("%w: entry %d term %d exceeds current term %d",
				errors.ErrRaftEntryTermExceedsCurrentTerm, e.Index, e.Term, currTerm)
		}
	}

	if err := CheckContiguity(entries, lastIdx+1); err != nil {
		return err
	}

	// 2. Encode all entries to buffer
	var totalBytes int
	for _, e := range entries {
		totalBytes += LogRecordHeaderSize + len(e.Data)
	}
	buf := make([]byte, totalBytes)
	offset := 0
	for _, e := range entries {
		encodedLen := encodeLogRecord(buf[offset:], e)
		offset += encodedLen
	}

	// 3. Write and sync to persistent storage media
	if _, err := s.logFile.Write(buf); err != nil {
		return fmt.Errorf("raft: failed to append log records to disk: %w", err)
	}
	if err := fdatasync(s.logFile); err != nil {
		return fmt.Errorf("raft: failed to sync log records to disk: %w", err)
	}

	// 4. Update in-memory log representation only after hardware durability barrier
	return s.memLog.Append(entries...)
}

// TruncateSuffix removes all entries starting at fromIndex and above both in-memory and durably on disk.
// Used when an AppendEntries consistency check detects divergent uncommitted entries.
//
// Operational lifecycle:
//   - If fromIndex > LastIndex(), no-op.
//   - Staged in `raft.log.tmp`, fsynced, and only upon success is the active descriptor closed
//     and swapped with os.Rename.
//   - If atomic rename or subsequent reopen fails, the storage enters a terminal fail-closed state.
//   - If staging or syncing the temp file fails, the original active logFile remains completely untouched.
func (s *Storage) TruncateSuffix(fromIndex LogIndex) error {
	if s.closed.Load() {
		return errors.ErrRaftStateClosed
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed.Load() {
		return errors.ErrRaftStateClosed
	}

	lastIdx := s.memLog.LastIndex()
	if fromIndex > lastIdx {
		return nil
	}

	var remaining []LogEntry
	if fromIndex > 1 {
		var err error
		remaining, err = s.memLog.Entries(1, fromIndex)
		if err != nil {
			return err
		}
	}

	tmpPath := filepath.Join(s.dir, LogTempFilename)
	logPath := filepath.Join(s.dir, LogFilename)

	// Clean up any stale tmp file
	_ = os.Remove(tmpPath)

	tmpFile, err := raftCreateTmpFn(tmpPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, RaftFileMode)
	if err != nil {
		return fmt.Errorf("raft: failed to create log tmp file: %w", err)
	}

	tmpCleaned := false
	defer func() {
		if !tmpCleaned {
			_ = raftCloseTmpFn(tmpFile)
			_ = os.Remove(tmpPath)
		}
	}()

	for _, e := range remaining {
		recBuf := make([]byte, LogRecordHeaderSize+len(e.Data))
		encodeLogRecord(recBuf, e)
		if _, err := raftWriteTmpFn(tmpFile, recBuf); err != nil {
			return fmt.Errorf("raft: failed to write truncated log record: %w", err)
		}
	}

	if err := raftSyncTmpFn(tmpFile); err != nil {
		return fmt.Errorf("raft: failed to sync truncated log file: %w", err)
	}

	if err := raftCloseTmpFn(tmpFile); err != nil {
		return fmt.Errorf("raft: failed to close truncated log tmp file: %w", err)
	}

	// At this point, tmpFile is complete and durably synced on disk.
	// Atomic rename to replace active log file.
	// If rename fails, old s.logFile was NEVER touched and remains valid.
	if err := raftRenameFn(tmpPath, logPath); err != nil {
		return fmt.Errorf("raft: failed to atomically replace log file: %w", err)
	}
	tmpCleaned = true

	// Disk state has changed. Any subsequent failure is terminal for this Storage instance.
	if err := raftSyncDirFn(s.dir); err != nil {
		s.closed.Store(true)
		if s.logFile != nil {
			_ = s.logFile.Close()
			s.logFile = nil
		}
		return fmt.Errorf("raft: failed to sync directory after log truncation: %w", err)
	}

	// Reopen replacement descriptor
	newLogFile, err := raftOpenFileFn(logPath, os.O_RDWR|os.O_APPEND, RaftFileMode)
	if err != nil {
		s.closed.Store(true)
		if s.logFile != nil {
			_ = s.logFile.Close()
			s.logFile = nil
		}
		return fmt.Errorf("raft: failed to reopen log file after truncation: %w", err)
	}

	// Only then retire old descriptor
	if s.logFile != nil {
		_ = s.logFile.Close()
	}
	s.logFile = newLogFile

	// Truncate in-memory log
	s.memLog.TruncateSuffix(fromIndex)
	return nil
}

// Close flushes and cleanly closes the persistent storage.
// Safe for concurrent and repeated invocation.
func (s *Storage) Close() error {
	if !s.closed.CompareAndSwap(false, true) {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var firstErr error
	if s.logFile != nil {
		if err := fdatasync(s.logFile); err != nil {
			firstErr = err
		}
		if err := s.logFile.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		s.logFile = nil
	}
	return firstErr
}

// --- Internal Binary Encoding & Persistence Helpers ---

// writeStateFile writes HardState atomically via temporary file staging, fdatasync, and os.Rename.
func writeStateFile(dir string, hs HardState) error {
	tmpPath := filepath.Join(dir, StateTempFilename)
	finalPath := filepath.Join(dir, StateFilename)

	// Clean up stale tmp file if present
	_ = os.Remove(tmpPath)

	tmpFile, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, RaftFileMode)
	if err != nil {
		return err
	}

	writeOk := false
	defer func() {
		if !writeOk {
			_ = tmpFile.Close()
			_ = os.Remove(tmpPath)
		}
	}()

	buf := make([]byte, StateRecordSize)
	binary.PutUint32(buf[0:4], StateMagic)
	binary.PutUint16(buf[4:6], StateVersion)
	binary.PutUint16(buf[6:8], 0) // Reserved
	binary.PutUint64(buf[8:16], uint64(hs.Term))
	binary.PutUint64(buf[16:24], uint64(hs.VotedFor))

	// CRC32-IEEE over the 24-byte payload
	crc := binary.Checksum(buf[0:StatePayloadSize])
	binary.PutUint32(buf[StatePayloadSize:StateRecordSize], crc)

	if _, err := tmpFile.Write(buf); err != nil {
		return err
	}
	if err := fdatasync(tmpFile); err != nil {
		return err
	}
	if err := tmpFile.Close(); err != nil {
		return err
	}

	if err := raftRenameFn(tmpPath, finalPath); err != nil {
		return err
	}
	writeOk = true

	return raftSyncDirFn(dir)
}

// readStateFile reads and validates the HardState metadata file.
func readStateFile(path string) (HardState, error) {
	f, err := os.Open(path)
	if err != nil {
		return HardState{}, err
	}
	defer func() { _ = f.Close() }()

	fi, err := f.Stat()
	if err != nil {
		return HardState{}, err
	}
	if fi.Size() != int64(StateRecordSize) {
		if fi.Size() < int64(StateRecordSize) {
			return HardState{}, fmt.Errorf("%w: state file truncated (%d bytes, expected %d)",
				errors.ErrRaftCorruptedState, fi.Size(), StateRecordSize)
		}
		return HardState{}, fmt.Errorf("%w: unexpected trailing bytes in state file (%d bytes, expected %d)",
			errors.ErrRaftCorruptedState, fi.Size(), StateRecordSize)
	}

	buf := make([]byte, StateRecordSize)
	if _, err := io.ReadFull(f, buf); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return HardState{}, fmt.Errorf("%w: state file truncated", errors.ErrRaftCorruptedState)
		}
		return HardState{}, err
	}

	extra := make([]byte, 1)
	if n, _ := f.Read(extra); n > 0 {
		return HardState{}, fmt.Errorf("%w: unexpected trailing bytes in state file", errors.ErrRaftCorruptedState)
	}

	// Check CRC32
	payload := buf[0:StatePayloadSize]
	expectedCRC := binary.GetUint32(buf[StatePayloadSize:StateRecordSize])
	if err := binary.VerifyChecksum(payload, expectedCRC); err != nil {
		return HardState{}, fmt.Errorf("%w: state file CRC mismatch: %v", errors.ErrRaftCorruptedState, err)
	}

	// Validate Magic
	magic := binary.GetUint32(buf[0:4])
	if magic != StateMagic {
		return HardState{}, fmt.Errorf("%w: invalid state magic 0x%08x", errors.ErrRaftCorruptedState, magic)
	}

	// Validate Version
	ver := binary.GetUint16(buf[4:6])
	if ver != StateVersion {
		return HardState{}, fmt.Errorf("%w: unsupported state version %d (expected %d)", errors.ErrRaftCorruptedState, ver, StateVersion)
	}

	term := Term(binary.GetUint64(buf[8:16]))
	votedFor := cluster.NodeID(binary.GetUint64(buf[16:24]))

	hs := HardState{
		Term:     term,
		VotedFor: votedFor,
	}
	if err := hs.Validate(); err != nil {
		return HardState{}, err
	}

	return hs, nil
}

// encodeLogRecord encodes a LogEntry into dst. Requires len(dst) >= LogRecordHeaderSize + len(e.Data).
// Framing layout:
//
//	Offset 0..3   : CRC32-IEEE (4B uint32) over buf[4..LogRecordHeaderSize+len(Data)]
//	Offset 4..11  : Index (8B uint64)
//	Offset 12..19 : Term (8B uint64)
//	Offset 20     : Type (1B uint8)
//	Offset 21..24 : DataLen (4B uint32)
//	Offset 25..   : Data (DataLen bytes)
func encodeLogRecord(dst []byte, e LogEntry) int {
	dataLen := uint32(len(e.Data))
	recLen := LogRecordHeaderSize + int(dataLen)

	binary.PutUint64(dst[4:12], uint64(e.Index))
	binary.PutUint64(dst[12:20], uint64(e.Term))
	dst[20] = byte(e.Type)
	binary.PutUint32(dst[21:25], dataLen)
	if dataLen > 0 {
		copy(dst[25:recLen], e.Data)
	}

	// CRC32 computed over entire payload after CRC offset (bytes 4..recLen)
	crc := binary.Checksum(dst[4:recLen])
	binary.PutUint32(dst[0:4], crc)

	return recLen
}

// recoverLogFile reads and validates log records from logPath.
// If an incomplete or torn write exists at physical EOF, it truncates the file back to the
// last valid record boundary and syncs the file.
func recoverLogFile(logPath string) ([]LogEntry, error) {
	f, err := os.OpenFile(logPath, os.O_RDWR, RaftFileMode)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var entries []LogEntry
	hdrBuf := make([]byte, LogRecordHeaderSize)
	var validOffset int64
	var lastIndex LogIndex

	for {
		// Read header
		n, err := io.ReadFull(f, hdrBuf)
		if err == io.EOF {
			// Clean physical end of file
			break
		}
		if err == io.ErrUnexpectedEOF {
			// Torn write at physical EOF: truncate file to validOffset
			if truncErr := f.Truncate(validOffset); truncErr != nil {
				return nil, fmt.Errorf("raft: failed to truncate torn write at EOF: %w", truncErr)
			}
			if syncErr := fdatasync(f); syncErr != nil {
				return nil, fmt.Errorf("raft: failed to sync after truncating torn write: %w", syncErr)
			}
			break
		}
		if err != nil {
			return nil, err
		}

		crc := binary.GetUint32(hdrBuf[0:4])
		idx := LogIndex(binary.GetUint64(hdrBuf[4:12]))
		term := Term(binary.GetUint64(hdrBuf[12:20]))
		entryType := transport.PeerEntryType(hdrBuf[20])
		dataLen := binary.GetUint32(hdrBuf[21:25])

		// Bound checking
		if dataLen > MaxLogEntryDataSize {
			return nil, fmt.Errorf("%w: record data length %d exceeds ceiling %d",
				errors.ErrRaftCorruptedState, dataLen, MaxLogEntryDataSize)
		}

		payloadBuf := make([]byte, dataLen)
		if dataLen > 0 {
			pn, perr := io.ReadFull(f, payloadBuf)
			if perr == io.EOF || perr == io.ErrUnexpectedEOF {
				// Torn write: truncate back to validOffset
				if truncErr := f.Truncate(validOffset); truncErr != nil {
					return nil, fmt.Errorf("raft: failed to truncate torn write at EOF: %w", truncErr)
				}
				if syncErr := fdatasync(f); syncErr != nil {
					return nil, fmt.Errorf("raft: failed to sync after truncating torn write: %w", syncErr)
				}
				break
			}
			if perr != nil {
				return nil, perr
			}
			_ = pn
		}

		// Verify CRC over header payload (bytes 4..25) + payloadBuf
		checkBuf := make([]byte, (LogRecordHeaderSize-4)+int(dataLen))
		copy(checkBuf[0:LogRecordHeaderSize-4], hdrBuf[4:LogRecordHeaderSize])
		if dataLen > 0 {
			copy(checkBuf[LogRecordHeaderSize-4:], payloadBuf)
		}
		if err := binary.VerifyChecksum(checkBuf, crc); err != nil {
			return nil, fmt.Errorf("%w: log record checksum mismatch at offset %d: %v", errors.ErrRaftCorruptedState, validOffset, err)
		}

		// Validate entry fields
		entry := LogEntry{
			Index: idx,
			Term:  term,
			Type:  entryType,
			Data:  payloadBuf,
		}
		if err := entry.Validate(); err != nil {
			return nil, fmt.Errorf("%w: log record validation failed at index %d: %v", errors.ErrRaftCorruptedState, idx, err)
		}

		// Validate contiguity
		expectedIdx := lastIndex + 1
		if idx != expectedIdx {
			return nil, fmt.Errorf("%w: index gap at offset %d: expected %d, got %d",
				errors.ErrRaftCorruptedState, validOffset, expectedIdx, idx)
		}

		entries = append(entries, entry)
		lastIndex = idx
		validOffset += int64(n) + int64(dataLen)
	}

	return entries, nil
}
