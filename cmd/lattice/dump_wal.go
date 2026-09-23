package main

import (
	stdErrors "errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/security"
	"github.com/silent-knight19/lattice/internal/wal"
)

const (
	// ExitDumpSuccess indicates successful inspection of a structurally valid WAL segment.
	ExitDumpSuccess = 0

	// ExitDumpUsageError indicates invalid CLI flags or missing arguments.
	ExitDumpUsageError = 1

	// ExitDumpFileError indicates file access errors (not found, permission denied, directory, symlink, not regular file).
	ExitDumpFileError = 2

	// ExitDumpCorruptError indicates that structural corruption, CRC verification failure, or torn tail was detected.
	ExitDumpCorruptError = 3

	// maxVerboseValuePreview limits the displayed value preview in verbose mode to 64 bytes.
	maxVerboseValuePreview = 64
)

// DumpWALReport captures summary statistics for a completed WAL inspection.
type DumpWALReport struct {
	Path         string
	FileSize     int64
	RecordCount  int
	FirstSeq     uint64
	LastSeq      uint64
	HasSeq       bool
	FinalOffset  int64
	Integrity    string
	IsCorrupt    bool
	CorruptRec   int
	CorruptOff   int64
	CorruptType  string
	CorruptError string
}

// DumpWAL streams and decodes every physical record in a WAL segment at path,
// printing a deterministic forensic report to stdout.
//
// Invariants:
//   - Strictly read-only file access; no file creation, modification, truncation, or repair.
//   - Streaming O(1) memory consumption; each record is formatted and discarded sequentially.
//   - Authoritative CRC32 verification through the existing WAL reader.
//   - Deterministic distinction between clean EOF (Exit 0) and truncated tails (Exit 3).
//   - Terminal-safe escaping of binary keys and values using FormatBytes.
func DumpWAL(path string, verbose bool, stdout, stderr io.Writer) int {
	cleanPath, err := security.CleanAndValidatePath(path)
	if err != nil {
		fmt.Fprintf(stderr, "lattice dump-wal: %v\n", err)
		return ExitDumpFileError
	}

	// Step 1: Open WAL reader (enforcing regular file, no symlinks, no dirs, inode pinning)
	r, err := wal.OpenReader(cleanPath)
	if err != nil {
		fmt.Fprintf(stderr, "lattice dump-wal: %v\n", err)
		return ExitDumpFileError
	}
	defer func() { _ = r.Close() }()

	// Step 2: Query file size for header reporting
	fileInfo, err := os.Stat(cleanPath)
	if err != nil {
		fmt.Fprintf(stderr, "lattice dump-wal: failed to stat %s: %v\n", cleanPath, err)
		return ExitDumpFileError
	}
	fileSize := fileInfo.Size()

	// Step 3: Print report header
	fmt.Fprintf(stdout, "===============================================================================\n")
	fmt.Fprintf(stdout, "LATTICE WAL FORENSIC REPORT\n")
	fmt.Fprintf(stdout, "===============================================================================\n")
	fmt.Fprintf(stdout, "File Path:        %s\n", cleanPath)
	fmt.Fprintf(stdout, "File Size:        %d bytes\n\n", fileSize)
	fmt.Fprintf(stdout, "RECORDS:\n")

	report := DumpWALReport{
		Path:      cleanPath,
		FileSize:  fileSize,
		Integrity: "VALID",
	}

	recordIndex := 0

	// Step 4: Stream records sequentially
	for {
		recOffset := r.Offset()
		rec, err := r.Next()
		if err != nil {
			if stdErrors.Is(err, io.EOF) {
				// Clean EOF at exact record boundary
				break
			}

			// Record-level corruption or truncation encountered
			report.IsCorrupt = true
			report.CorruptRec = recordIndex
			report.CorruptOff = recOffset
			report.FinalOffset = recOffset

			classifyWALError(err, &report)
			break
		}

		// Record successfully decoded and CRC verified
		recLen := wal.MinRecordSize + len(rec.Key) + len(rec.Value)

		fmt.Fprintf(stdout, "  Record #%d:\n", recordIndex)
		fmt.Fprintf(stdout, "    Offset:       %d bytes\n", recOffset)
		fmt.Fprintf(stdout, "    Length:       %d bytes\n", recLen)
		fmt.Fprintf(stdout, "    Type:         %s\n", rec.Type.String())
		fmt.Fprintf(stdout, "    Sequence:     %d\n", uint64(rec.SeqNum))
		fmt.Fprintf(stdout, "    Timestamp:    %d\n", rec.Timestamp)
		fmt.Fprintf(stdout, "    Key Length:   %d\n", len(rec.Key))
		fmt.Fprintf(stdout, "    Value Length: %d\n", len(rec.Value))
		fmt.Fprintf(stdout, "    CRC:          0x%08x [PASS]\n", rec.CRC)

		if verbose {
			fmt.Fprintf(stdout, "    Key:          %s\n", FormatBytes(rec.Key))
			if len(rec.Value) <= maxVerboseValuePreview {
				fmt.Fprintf(stdout, "    Value:        %s\n", FormatBytes(rec.Value))
			} else {
				preview := FormatBytes(rec.Value[:maxVerboseValuePreview])
				fmt.Fprintf(stdout, "    Value:        %s... (%d bytes total)\n", preview, len(rec.Value))
			}
		}
		fmt.Fprintln(stdout)

		// Update running sequence statistics
		if !report.HasSeq {
			report.FirstSeq = uint64(rec.SeqNum)
			report.HasSeq = true
		}
		report.LastSeq = uint64(rec.SeqNum)
		report.FinalOffset = r.Offset()
		report.RecordCount++
		recordIndex++
	}

	if report.RecordCount == 0 && !report.IsCorrupt {
		fmt.Fprintf(stdout, "  (Empty WAL segment - 0 records)\n\n")
	}

	// Step 5: Report corruption / truncation if present
	if report.IsCorrupt {
		fmt.Fprintf(stdout, "INTEGRITY STATUS: %s\n\n", report.Integrity)
		fmt.Fprintf(stdout, "Corruption:\n")
		fmt.Fprintf(stdout, "  Record:         %d\n", report.CorruptRec)
		fmt.Fprintf(stdout, "  Offset:         %d bytes\n", report.CorruptOff)
		fmt.Fprintf(stdout, "  Type:           %s\n", report.CorruptType)
		fmt.Fprintf(stdout, "  Error:          %s\n\n", report.CorruptError)
	}

	// Step 6: Print report summary
	firstSeqStr := "N/A"
	lastSeqStr := "N/A"
	if report.HasSeq {
		firstSeqStr = fmt.Sprintf("%d", report.FirstSeq)
		lastSeqStr = fmt.Sprintf("%d", report.LastSeq)
	}

	fmt.Fprintf(stdout, "SUMMARY:\n")
	fmt.Fprintf(stdout, "  Records:        %d\n", report.RecordCount)
	fmt.Fprintf(stdout, "  First Sequence: %s\n", firstSeqStr)
	fmt.Fprintf(stdout, "  Last Sequence:  %s\n", lastSeqStr)
	fmt.Fprintf(stdout, "  Final Offset:   %d bytes\n", report.FinalOffset)
	fmt.Fprintf(stdout, "  Integrity:      %s\n", report.Integrity)
	fmt.Fprintf(stdout, "===============================================================================\n")

	if report.IsCorrupt {
		return ExitDumpCorruptError
	}
	return ExitDumpSuccess
}

// classifyWALError categorizes corruption, CRC mismatch, or torn tails into forensic report fields.
func classifyWALError(err error, report *DumpWALReport) {
	var crcErr *errors.ChecksumMismatchError
	var typeErr *errors.InvalidRecordTypeError
	var keyErr *errors.KeyTooLargeError
	var valErr *errors.ValueTooLargeError
	var payloadErr *errors.InvalidRecordPayloadError

	switch {
	case stdErrors.As(err, &crcErr):
		report.Integrity = "CORRUPT"
		report.CorruptType = "CRC MISMATCH"
		report.CorruptError = fmt.Sprintf("expected 0x%08x, calculated 0x%08x", crcErr.Expected, crcErr.Actual)

	case stdErrors.Is(err, errors.ErrHeaderTruncated) || stdErrors.Is(err, io.ErrUnexpectedEOF):
		report.Integrity = "TRUNCATED"
		report.CorruptType = "TRUNCATED RECORD"
		report.CorruptError = err.Error()

	case stdErrors.As(err, &typeErr):
		report.Integrity = "CORRUPT"
		report.CorruptType = "INVALID RECORD TYPE"
		report.CorruptError = fmt.Sprintf("unrecognized record type 0x%02x", typeErr.Type)

	case stdErrors.Is(err, errors.ErrEmptyKey):
		report.Integrity = "CORRUPT"
		report.CorruptType = "EMPTY KEY"
		report.CorruptError = "put or delete record has zero-length key"

	case stdErrors.As(err, &keyErr):
		report.Integrity = "CORRUPT"
		report.CorruptType = "KEY TOO LARGE"
		report.CorruptError = fmt.Sprintf("key size %d exceeds max %d", keyErr.KeySize, keyErr.MaxSize)

	case stdErrors.As(err, &valErr):
		report.Integrity = "CORRUPT"
		report.CorruptType = "VALUE TOO LARGE"
		report.CorruptError = fmt.Sprintf("value size %d exceeds max %d", valErr.ValueSize, valErr.MaxSize)

	case stdErrors.As(err, &payloadErr):
		report.Integrity = "CORRUPT"
		report.CorruptType = "INVALID PAYLOAD"
		report.CorruptError = fmt.Sprintf("invalid payload for type 0x%02x: %s", payloadErr.Type, payloadErr.Reason)

	default:
		report.Integrity = "CORRUPT"
		report.CorruptType = "STRUCTURAL CORRUPTION"
		report.CorruptError = err.Error()
	}
}

// runDumpWAL parses CLI flags and dispatches DumpWAL.
func runDumpWAL(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("dump-wal", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		flagVerbose      bool
		flagVerboseShort bool
		flagHelp         bool
		flagHelpShort    bool
		flagVersion      bool
		flagVersionShort bool
	)

	fs.BoolVar(&flagVerbose, "verbose", false, "Print detailed record contents including keys and preview values")
	fs.BoolVar(&flagVerboseShort, "v", false, "Print detailed record contents including keys and preview values")
	fs.BoolVar(&flagHelp, "help", false, "Display usage instructions and exit")
	fs.BoolVar(&flagHelpShort, "h", false, "Display usage instructions and exit")
	fs.BoolVar(&flagVersion, "version", false, "Display version information and exit")
	fs.BoolVar(&flagVersionShort, "V", false, "Display version information and exit")

	fs.Usage = func() {
		fmt.Fprintf(stdout, "Usage of lattice dump-wal:\n")
		fmt.Fprintf(stdout, "  lattice dump-wal [flags] <wal-path>\n\n")
		fmt.Fprintf(stdout, "Flags:\n")
		fs.SetOutput(stdout)
		fs.PrintDefaults()
		fs.SetOutput(stderr)
	}

	if err := fs.Parse(args); err != nil {
		if stdErrors.Is(err, flag.ErrHelp) {
			return ExitDumpSuccess
		}
		return ExitDumpUsageError
	}

	if flagHelp || flagHelpShort {
		fs.Usage()
		return ExitDumpSuccess
	}

	if flagVersion || flagVersionShort {
		fmt.Fprintf(stdout, "lattice dump-wal version %s\n", Version)
		return ExitDumpSuccess
	}

	remaining := fs.Args()
	if len(remaining) == 0 {
		fmt.Fprintf(stderr, "lattice dump-wal: missing WAL segment file path\n")
		fs.Usage()
		return ExitDumpUsageError
	}
	if len(remaining) > 1 {
		fmt.Fprintf(stderr, "lattice dump-wal: too many arguments provided (expected 1, got %d)\n", len(remaining))
		fs.Usage()
		return ExitDumpUsageError
	}

	walPath := remaining[0]
	return DumpWAL(walPath, flagVerbose || flagVerboseShort, stdout, stderr)
}
