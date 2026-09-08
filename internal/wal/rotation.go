package wal

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/silent-knight19/lattice/internal/errors"
)

// DefaultSegmentSize is the standard maximum size (64 MiB) of a WAL segment file
// before rotation is triggered, adhering to docs/architecture-spec.md (Section 20)
// and docs/implementation-plan.md (Sub-Phase 02.3 M02).
const DefaultSegmentSize int64 = 64 * 1024 * 1024 // 64 MiB

// DefaultInitialSegmentID is the default first segment ID (1) for a newly initialized WAL.
const DefaultInitialSegmentID uint64 = 1

// SegmentFilenamePrefix is the canonical prefix for WAL segment files.
const SegmentFilenamePrefix = "wal_"

// SegmentFilenameSuffix is the canonical file extension for WAL segment files.
const SegmentFilenameSuffix = ".log"

// SegmentFilenameLen is the standard character length of a 12-digit WAL segment filename:
// len("wal_") [4] + 12 digits [12] + len(".log") [4] = 20.
const SegmentFilenameLen = 20

// Options configures the behavior and boundary thresholds of a RotatingWriter.
type Options struct {
	// SegmentSize is the soft byte-size threshold at which an active segment is sealed
	// and rotated to a new segment. If <= 0, DefaultSegmentSize (64 MiB) is used.
	SegmentSize int64

	// InitialSegmentID is the starting segment ID used when initializing an empty WAL directory.
	// If <= 0, DefaultInitialSegmentID (1) is used.
	InitialSegmentID uint64
}

// RecordWireSize returns the exact physical serialized wire length in bytes of a Record.
// Wire size = MinRecordSize (27) + len(Key) + len(Value).
func RecordWireSize(rec Record) int64 {
	return int64(MinRecordSize + len(rec.Key) + len(rec.Value))
}

// ParseSegmentID parses a canonical WAL segment filename (e.g. "wal_000000000001.log")
// and returns its numeric segment ID.
//
// Format Requirements:
//   - Must start with prefix "wal_"
//   - Must end with suffix ".log"
//   - Must contain between 12 and 20 ASCII decimal digits between prefix and suffix
//   - Filenames with > 12 digits must not contain superfluous leading zeros
//   - Segment ID must be strictly positive (>= 1, ID 0 is strictly rejected)
//
// Returns an error wrapping os.ErrInvalid if the filename does not strictly conform.
func ParseSegmentID(name string) (uint64, error) {
	if len(name) < SegmentFilenameLen {
		return 0, fmt.Errorf("wal: invalid segment filename %q (length %d, expected >= %d): %w", name, len(name), SegmentFilenameLen, os.ErrInvalid)
	}
	if !strings.HasPrefix(name, SegmentFilenamePrefix) {
		return 0, fmt.Errorf("wal: invalid segment filename prefix %q: %w", name, os.ErrInvalid)
	}
	if !strings.HasSuffix(name, SegmentFilenameSuffix) {
		return 0, fmt.Errorf("wal: invalid segment filename suffix %q: %w", name, os.ErrInvalid)
	}

	digitStr := name[len(SegmentFilenamePrefix) : len(name)-len(SegmentFilenameSuffix)]
	if len(digitStr) < 12 || len(digitStr) > 20 {
		return 0, fmt.Errorf("wal: invalid segment filename digit count %d in %q: %w", len(digitStr), name, os.ErrInvalid)
	}
	if len(digitStr) > 12 && digitStr[0] == '0' {
		return 0, fmt.Errorf("wal: segment filename %q has superfluous leading zeros: %w", name, os.ErrInvalid)
	}
	for i := 0; i < len(digitStr); i++ {
		if digitStr[i] < '0' || digitStr[i] > '9' {
			return 0, fmt.Errorf("wal: non-digit character %q in segment filename %q: %w", digitStr[i], name, os.ErrInvalid)
		}
	}

	id, err := strconv.ParseUint(digitStr, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("wal: failed to parse segment ID in %q: %w", name, err)
	}
	if id == 0 {
		return 0, fmt.Errorf("wal: segment ID in %q must be strictly positive: %w", name, os.ErrInvalid)
	}

	return id, nil
}

// ParseSegmentPath extracts the basename of path and parses its numeric segment ID via ParseSegmentID.
func ParseSegmentPath(path string) (uint64, error) {
	return ParseSegmentID(filepath.Base(path))
}

// ListSegments scans the WAL directory under dbPath and returns all valid segment IDs
// sorted in strictly ascending numeric order (e.g. 1 -> 2 -> 3).
//
// Invariants:
//   - Non-segment files (e.g. temporary files, manifests) and subdirectories are ignored.
//   - Ordering is based strictly on parsed numeric integer value, NOT lexicographical sort.
//   - Returns an empty slice (nil error) if the directory contains no segment files or does not exist.
func ListSegments(dbPath string) ([]uint64, error) {
	walDir := Dir(dbPath)
	entries, err := os.ReadDir(walDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("wal: failed to read directory %s: %w", walDir, err)
	}

	var ids []uint64
	for _, entry := range entries {
		id, err := ParseSegmentID(entry.Name())
		if err != nil {
			// Non-segment file or unrelated subdirectory in wal directory; ignore
			continue
		}

		fullPath := filepath.Join(walDir, entry.Name())
		info, statErr := os.Lstat(fullPath)
		if statErr != nil {
			return nil, fmt.Errorf("wal: failed to inspect segment %s: %w", fullPath, statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("wal: cannot open symlink %s: %w", fullPath, os.ErrInvalid)
		}
		if info.IsDir() {
			return nil, &errors.NotADirectoryError{
				Path: fullPath,
				Mode: info.Mode(),
			}
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("wal: path %s is not a regular file (mode: %s): %w", fullPath, info.Mode(), os.ErrInvalid)
		}

		ids = append(ids, id)
	}

	// Explicit numeric ascending sort: guarantees ordering 1 -> 2 -> 3
	sort.Slice(ids, func(i, j int) bool {
		return ids[i] < ids[j]
	})

	return ids, nil
}

// RotatingWriter manages an ordered sequence of WAL segment files under a database directory,
// automatically rotating to a newly created segment (N+1) when the active segment reaches
// the configured size threshold.
//
// Core Invariants & Lifecycle Guarantees:
//  1. Single Active Writer:
//     Exclusively owns exactly one active WALWriter at any given time.
//  2. Monotonic Segment IDs:
//     Segment IDs increment strictly monotonically (1 -> 2 -> 3).
//  3. Atomic Segment Creation & Collision Defense:
//     New segments are created with os.O_WRONLY | os.O_CREATE | os.O_EXCL | os.O_APPEND.
//     If a file already exists at the target path, rotation fails with os.ErrExist without
//     overwriting, truncating, or adopting the foreign file.
//  4. Pre-Write Rotation Trigger:
//     Rotation is evaluated before appending each record: if activeLen > 0 and
//     activeLen + recWireSize > SegmentSize, rotation seals the active segment first.
//  5. Oversized Record Policy:
//     If the active segment is empty (activeLen == 0) and an incoming record exceeds SegmentSize,
//     the record is accepted into that segment (preventing deadlock). Subsequent records will
//     immediately trigger rotation.
//  6. Concurrency Safety:
//     Thread-safe under -race. An internal mutex serializes concurrent AppendSync calls and
//     guarantees that only one goroutine executes rotation. No records can be appended to a
//     closed previous segment.
//  7. Independent Segment Readability:
//     Closed previous segments remain completely intact on disk, terminated cleanly at io.EOF,
//     and independently readable via WALReader.
type RotatingWriter struct {
	mu        sync.Mutex
	dbPath    string
	opts      Options
	active    *WALWriter
	activeID  uint64
	activeLen int64
	closed    bool

	// createWriterFn is an internal test seam for injecting creation failures.
	createWriterFn func(path string) (*WALWriter, error)
}

// WAL is an alias for RotatingWriter, matching standard database engine terminology.
type WAL = RotatingWriter

// OpenRotatingWriter opens or initializes a rotating WAL writer under dbPath with the given options.
//
// If no segments exist in dbPath/wal, segment InitialSegmentID (default 1) is created.
// If segments already exist, the segment with the highest numeric ID is opened for sequential appending.
func OpenRotatingWriter(dbPath string, opts Options) (*RotatingWriter, error) {
	if dbPath == "" {
		return nil, fmt.Errorf("%w: db path cannot be empty", os.ErrInvalid)
	}

	cleanDBPath := filepath.Clean(dbPath)

	// Ensure WAL directory is safely initialized with 0700 permissions
	if _, err := InitDir(cleanDBPath); err != nil {
		return nil, fmt.Errorf("wal: failed to initialize directory for rotating writer: %w", err)
	}

	// Normalize configuration options
	if opts.SegmentSize <= 0 {
		opts.SegmentSize = DefaultSegmentSize
	}
	if opts.InitialSegmentID <= 0 {
		opts.InitialSegmentID = DefaultInitialSegmentID
	}

	// Discover existing segments in the WAL directory
	existingIDs, err := ListSegments(cleanDBPath)
	if err != nil {
		return nil, fmt.Errorf("wal: failed to discover existing segments: %w", err)
	}

	var (
		activeWriter *WALWriter
		activeID     uint64
		activeLen    int64
	)

	if len(existingIDs) == 0 {
		// Fresh WAL directory: create initial segment with exclusive creation
		activeID = opts.InitialSegmentID
		w, err := CreateSegmentWriter(cleanDBPath, activeID)
		if err != nil {
			return nil, fmt.Errorf("wal: failed to create initial segment %d: %w", activeID, err)
		}
		activeWriter = w
		activeLen = 0
	} else {
		// Existing segments found: resume the highest numbered active segment
		activeID = existingIDs[len(existingIDs)-1]
		w, err := OpenSegmentWriter(cleanDBPath, activeID)
		if err != nil {
			return nil, fmt.Errorf("wal: failed to resume active segment %d: %w", activeID, err)
		}
		size, err := w.Size()
		if err != nil {
			_ = w.Close()
			return nil, fmt.Errorf("wal: failed to determine size of resumed segment %d: %w", activeID, err)
		}
		activeWriter = w
		activeLen = size
	}

	rw := &RotatingWriter{
		dbPath:         cleanDBPath,
		opts:           opts,
		active:         activeWriter,
		activeID:       activeID,
		activeLen:      activeLen,
		closed:         false,
		createWriterFn: CreateWriter,
	}

	return rw, nil
}

// Open opens a rotating WAL writer under dbPath with the given options.
// Alias for OpenRotatingWriter.
func Open(dbPath string, opts Options) (*RotatingWriter, error) {
	return OpenRotatingWriter(dbPath, opts)
}

// ActiveSegmentID returns the numeric ID of the currently active segment file.
// Returns 0 if no segment is currently active (e.g. after a failed rotation or after close).
func (rw *RotatingWriter) ActiveSegmentID() uint64 {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	if rw.active == nil || rw.closed {
		return 0
	}
	return rw.activeID
}

// ActiveSegmentSize returns the current physical byte length of the active segment file.
// Returns 0 if no segment is currently active.
func (rw *RotatingWriter) ActiveSegmentSize() int64 {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	if rw.active == nil || rw.closed {
		return 0
	}
	return rw.activeLen
}

// ActivePath returns the filesystem path of the currently active segment file.
// Returns empty string if no segment is currently active.
func (rw *RotatingWriter) ActivePath() string {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	if rw.active == nil || rw.closed {
		return ""
	}
	return rw.active.Path()
}

// DBPath returns the database root directory path containing the WAL directory.
func (rw *RotatingWriter) DBPath() string {
	return rw.dbPath
}

// ActiveWriter returns the underlying *WALWriter for the active segment.
// Returns nil if no segment is active.
func (rw *RotatingWriter) ActiveWriter() *WALWriter {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	return rw.active
}

// Append appends record to the active segment without executing an fdatasync durability barrier.
//
// Invariants & Operational Flow:
//   - Thread-safe under concurrent callers.
//   - If writer is closed, returns errors.ErrWriterClosed.
//   - If record validation fails, returns the validation error without file mutation.
//   - Evaluates rotation threshold before appending: if activeLen > 0 and
//     activeLen + recWireSize > SegmentSize, triggers rotation to segment N+1.
//     During rotation, the old segment is closed and synced to disk.
//   - Appends record to the active segment via WALWriter.Append.
//   - On success, updates active segment size by the physical wire length.
//   - Does NOT guarantee durability of the new record until Sync() is called.
func (rw *RotatingWriter) Append(rec Record) error {
	rw.mu.Lock()
	defer rw.mu.Unlock()

	if rw.closed {
		return errors.ErrWriterClosed
	}
	if rw.active == nil {
		return fmt.Errorf("wal: writer has no active segment (previous rotation failed)")
	}

	return rw.appendLocked(rec)
}

// Sync synchronizes the currently active segment file descriptor to non-volatile storage.
//
// Invariants:
//   - If the writer is closed, returns errors.ErrWriterClosed.
//   - If no segment is active, returns an error.
//   - Thread-safe under concurrent callers.
func (rw *RotatingWriter) Sync() error {
	rw.mu.Lock()
	defer rw.mu.Unlock()

	if rw.closed {
		return errors.ErrWriterClosed
	}
	if rw.active == nil {
		return fmt.Errorf("wal: writer has no active segment")
	}

	return rw.active.Sync()
}

func (rw *RotatingWriter) appendLocked(rec Record) error {
	// Validate record invariants before touching disk or rotating
	if err := rec.Validate(); err != nil {
		return err
	}

	recWireSize := RecordWireSize(rec)

	// Rotation Condition:
	// If the current segment has existing records and appending this record would
	// exceed the configured segment boundary, rotate to next segment first.
	// If the active segment is already at math.MaxUint64, rotation is prohibited
	// (segment ID cannot overflow to 0), so the record is appended to the current segment.
	// If the current segment is empty (activeLen == 0), the record is accepted into
	// the empty segment (oversized record policy) to prevent infinite rotation deadlocks.
	if rw.activeLen > 0 && (rw.activeLen+recWireSize > rw.opts.SegmentSize) {
		if rw.activeID < math.MaxUint64 {
			if err := rw.rotateLocked(); err != nil {
				return fmt.Errorf("wal: rotation triggered by append failed: %w", err)
			}
		}
	}

	// Append record to active segment without sync
	if err := rw.active.Append(rec); err != nil {
		// Update physical size tracking on error in case partial write occurred
		if sz, szErr := rw.active.Size(); szErr == nil {
			rw.activeLen = sz
		}
		return err
	}

	rw.activeLen += recWireSize
	return nil
}

// AppendSync appends record to the active segment with immediate hardware durability.
//
// Invariants & Operational Flow:
//   - Thread-safe under concurrent callers.
//   - If writer is closed, returns errors.ErrWriterClosed.
//   - If record validation fails, returns the validation error without file mutation.
//   - Evaluates rotation threshold before appending: if activeLen > 0 and
//     activeLen + recWireSize > SegmentSize, triggers rotation to segment N+1.
//   - Appends record to the active segment via WALWriter.Append.
//   - On success, updates active segment size by the physical wire length and executes Sync().
//   - On failure, queries file stat to maintain accurate size tracking and returns error.
func (rw *RotatingWriter) AppendSync(rec Record) error {
	rw.mu.Lock()
	defer rw.mu.Unlock()

	if rw.closed {
		return errors.ErrWriterClosed
	}
	if rw.active == nil {
		return fmt.Errorf("wal: writer has no active segment (previous rotation failed)")
	}

	if err := rw.appendLocked(rec); err != nil {
		return err
	}

	return rw.active.Sync()
}

// Rotate explicitly seals the current active segment and transitions to segment N+1.
//
// Lifecycle:
//  1. Closes and syncs current segment N.
//  2. Atomically creates segment N+1 with exclusive creation (O_EXCL).
//  3. Transitions active writer handle to segment N+1.
//  4. If creation of N+1 fails, active writer is set to nil and error is returned.
func (rw *RotatingWriter) Rotate() error {
	rw.mu.Lock()
	defer rw.mu.Unlock()

	if rw.closed {
		return errors.ErrWriterClosed
	}

	return rw.rotateLocked()
}

// rotateLocked executes segment rotation while holding rw.mu.
func (rw *RotatingWriter) rotateLocked() error {
	if rw.closed {
		return errors.ErrWriterClosed
	}

	oldWriter := rw.active
	oldID := rw.activeID

	// Guard against segment ID overflow: math.MaxUint64 + 1 wraps to 0.
	// Rotation is rejected BEFORE closing the active writer or touching the filesystem.
	// The active writer remains usable and activeID remains math.MaxUint64.
	if oldID == math.MaxUint64 {
		return &errors.SegmentIDOverflowError{Current: oldID}
	}

	nextID := oldID + 1

	// Step 1: Seal, flush, and close current active segment
	if oldWriter != nil {
		if err := oldWriter.Close(); err != nil {
			return fmt.Errorf("wal: failed to close segment %d during rotation: %w", oldID, err)
		}
	}

	// Step 2: Create next segment N+1 with atomic exclusive creation
	nextPath := SegmentPath(rw.dbPath, nextID)
	newWriter, err := rw.createWriterFn(nextPath)
	if err != nil {
		// Crucial invariant: A failed rotation must not silently report the new segment as active.
		// Mark active as nil so subsequent appends cannot corrupt or write to closed segments.
		rw.active = nil
		return fmt.Errorf("wal: failed to create next segment %d at %s: %w", nextID, nextPath, err)
	}

	// Step 3: Transition active state to new segment
	rw.active = newWriter
	rw.activeID = nextID
	rw.activeLen = 0
	return nil
}

// Close flushes data, executes the durability barrier on the active segment,
// and closes the underlying descriptor. Subsequent operations return errors.ErrWriterClosed.
// Close is idempotent; subsequent calls return nil.
func (rw *RotatingWriter) Close() error {
	rw.mu.Lock()
	defer rw.mu.Unlock()

	if rw.closed {
		return nil
	}
	rw.closed = true

	if rw.active != nil {
		err := rw.active.Close()
		rw.active = nil
		return err
	}
	return nil
}

// Segments returns all segment IDs discovered under the database WAL directory,
// sorted in strictly ascending numeric order.
func (rw *RotatingWriter) Segments() ([]uint64, error) {
	return ListSegments(rw.dbPath)
}
