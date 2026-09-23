package main

import (
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/filter"
	"github.com/silent-knight19/lattice/internal/security"
	"github.com/silent-knight19/lattice/internal/sstable"
)

const (
	// ExitInspectSuccess indicates successful inspection of a structurally valid SSTable.
	ExitInspectSuccess = 0

	// ExitInspectUsageError indicates invalid CLI flags or missing arguments.
	ExitInspectUsageError = 1

	// ExitInspectFileError indicates file access errors (not found, permission denied, not a regular file).
	ExitInspectFileError = 2

	// ExitInspectCorruptError indicates that structural corruption or CRC verification failure was detected.
	ExitInspectCorruptError = 3
)

// DataBlockInfo holds forensic details for a single data block.
type DataBlockInfo struct {
	Index        int
	Offset       uint64
	Size         uint64
	StoredCRC    uint32
	ComputedCRC  uint32
	CRCPass      bool
	RecordCount  int
	RestartCount int
	FirstKey     []byte
	LastKey      []byte
	FirstKeyIK   binary.InternalKey
	LastKeyIK    binary.InternalKey
	Restarts     []uint32
	Error        string
}

// BloomFilterInfo holds forensic details for the Bloom filter block.
type BloomFilterInfo struct {
	Present     bool
	Offset      uint64
	Size        uint64
	StoredCRC   uint32
	ComputedCRC uint32
	CRCPass     bool
	BitCount    uint64
	HashCount   int
	KeyCount    int
	ByteSize    int
	Error       string
}

// ForensicReport aggregates all structural metadata decoded from an SSTable.
type ForensicReport struct {
	Path           string
	FileSize       int64
	Valid          bool
	CorruptionNote string

	// Footer
	FooterMagic  uint64
	MagicValid   bool
	PaddingValid bool
	IndexHandle  sstable.BlockHandle
	MetaHandle   sstable.BlockHandle
	FooterError  string

	// Index
	IndexBlockSize uint64
	IndexEntryCnt  int
	IndexStoredCRC uint32
	IndexCompCRC   uint32
	IndexCRCPass   bool
	IndexError     string

	// Meta-Index
	MetaPresent   bool
	MetaSize      uint64
	MetaEntryCnt  int
	MetaStoredCRC uint32
	MetaCompCRC   uint32
	MetaCRCPass   bool
	MetaError     string

	// Filter
	Filter BloomFilterInfo

	// Data Blocks
	DataBlocks []DataBlockInfo
	TotalRecs  int
	FirstKey   []byte
	LastKey    []byte
	FirstKeyIK binary.InternalKey
	LastKeyIK  binary.InternalKey
	HasKeys    bool
}

// FormatBytes formats a byte slice safely for terminal display, escaping non-printable
// characters with \xHH to prevent terminal escape sequence injection.
func FormatBytes(b []byte) string {
	if len(b) == 0 {
		return `""`
	}
	isPrintable := true
	for i := 0; i < len(b); {
		r, size := utf8.DecodeRune(b[i:])
		if r == utf8.RuneError && size == 1 {
			isPrintable = false
			break
		}
		if !strconv.IsPrint(r) && r != '\n' && r != '\r' && r != '\t' {
			isPrintable = false
			break
		}
		i += size
	}
	if isPrintable {
		return fmt.Sprintf("%q", string(b))
	}
	var sb strings.Builder
	sb.WriteByte('"')
	for _, by := range b {
		if by >= 0x20 && by < 0x7f && by != '"' && by != '\\' {
			sb.WriteByte(by)
		} else {
			fmt.Fprintf(&sb, `\x%02x`, by)
		}
	}
	sb.WriteByte('"')
	return sb.String()
}

// InspectSSTable performs a read-only forensic inspection of an SSTable file at path.
// It populates a ForensicReport without mutating the file or initializing the storage Engine.
func InspectSSTable(path string, report *ForensicReport) error {
	cleanPath, err := security.CleanAndValidatePath(path)
	if err != nil {
		return err
	}

	// Pre-open stat: verify target exists and is a regular file before calling os.Open.
	// This prevents blocking indefinitely on named pipes (FIFOs) waiting for a writer (SEC-P12-001)
	// and prevents symlink redirection attacks.
	info, err := os.Lstat(cleanPath)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("path %q is a symbolic link: %w", cleanPath, os.ErrInvalid)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("path %q is not a regular file (mode: %s)", cleanPath, info.Mode())
	}

	file, err := os.Open(cleanPath)
	if err != nil {
		return err
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		return err
	}
	if !stat.Mode().IsRegular() {
		return fmt.Errorf("path %q is not a regular file (mode: %s)", cleanPath, stat.Mode())
	}
	if !os.SameFile(info, stat) {
		return fmt.Errorf("path %q was replaced during open: %w", cleanPath, os.ErrInvalid)
	}

	report.Path = cleanPath
	report.FileSize = stat.Size()
	fileSize := stat.Size()

	if fileSize < int64(sstable.FooterSize) {
		report.Valid = false
		report.CorruptionNote = fmt.Sprintf("file size (%d bytes) is smaller than minimum 48-byte footer", fileSize)
		return nil
	}

	// 1. Read and validate Footer (48 bytes from tail of file)
	var footerBuf [sstable.FooterSize]byte
	footerOffset := fileSize - int64(sstable.FooterSize)
	if _, err := file.ReadAt(footerBuf[:], footerOffset); err != nil {
		report.Valid = false
		report.CorruptionNote = fmt.Sprintf("failed to read footer: %v", err)
		return nil
	}

	magic := binary.GetUint64(footerBuf[40:48])
	report.FooterMagic = magic
	report.MagicValid = (magic == sstable.FooterMagic)
	if !report.MagicValid {
		report.Valid = false
		report.CorruptionNote = fmt.Sprintf("footer magic mismatch: expected 0x%016x, got 0x%016x", sstable.FooterMagic, magic)
		return nil
	}

	padding := binary.GetUint64(footerBuf[32:40])
	report.PaddingValid = (padding == 0)
	if !report.PaddingValid {
		report.Valid = false
		report.CorruptionNote = fmt.Sprintf("footer padding non-zero: 0x%016x", padding)
		return nil
	}

	footer, err := sstable.DecodeFooter(footerBuf[:])
	if err != nil {
		report.Valid = false
		report.CorruptionNote = fmt.Sprintf("failed to decode footer: %v", err)
		return nil
	}
	report.IndexHandle = footer.IndexHandle
	report.MetaHandle = footer.MetaIndexHandle

	if err := footer.ValidateAgainstFileSize(fileSize); err != nil {
		report.Valid = false
		report.CorruptionNote = fmt.Sprintf("footer block handles exceed file bounds: %v", err)
		return nil
	}

	// 2. Read and decode Index Block
	indexHandle := footer.IndexHandle
	if indexHandle.Size > sstable.MaxIndexBlockSize {
		report.Valid = false
		report.CorruptionNote = fmt.Sprintf("index handle size %d exceeds MaxIndexBlockSize %d", indexHandle.Size, sstable.MaxIndexBlockSize)
		return nil
	}
	if indexHandle.Offset > math.MaxUint64-indexHandle.Size || indexHandle.Offset+indexHandle.Size > uint64(fileSize)-sstable.FooterSize {
		report.Valid = false
		report.CorruptionNote = fmt.Sprintf("index handle [%d, %d] exceeds file boundary or extends into footer", indexHandle.Offset, indexHandle.Size)
		return nil
	}

	indexBuf := make([]byte, int(indexHandle.Size))
	if _, err := file.ReadAt(indexBuf, int64(indexHandle.Offset)); err != nil {
		report.Valid = false
		report.CorruptionNote = fmt.Sprintf("failed to read index block: %v", err)
		return nil
	}
	report.IndexBlockSize = indexHandle.Size

	if len(indexBuf) >= 4 {
		report.IndexStoredCRC = binary.GetUint32(indexBuf[len(indexBuf)-4:])
		report.IndexCompCRC = binary.Checksum(indexBuf[:len(indexBuf)-4])
		report.IndexCRCPass = (report.IndexStoredCRC == report.IndexCompCRC)
		if !report.IndexCRCPass {
			report.Valid = false
			report.CorruptionNote = fmt.Sprintf("index block CRC mismatch: stored 0x%08x, computed 0x%08x", report.IndexStoredCRC, report.IndexCompCRC)
			return nil
		}
	}

	blockIndex, err := sstable.DecodeBlockIndex(indexBuf)
	if err != nil {
		report.Valid = false
		report.CorruptionNote = fmt.Sprintf("failed to decode block index: %v", err)
		return nil
	}
	report.IndexEntryCnt = blockIndex.EntryCount()

	// 3. Read and decode Meta-Index & Bloom Filter
	metaHandle := footer.MetaIndexHandle
	if metaHandle.Size > 0 {
		report.MetaPresent = true
		report.MetaSize = metaHandle.Size
		if metaHandle.Size <= sstable.MaxBlockSize && metaHandle.Offset <= math.MaxUint64-metaHandle.Size && metaHandle.Offset+metaHandle.Size <= uint64(fileSize)-sstable.FooterSize {
			metaBuf := make([]byte, int(metaHandle.Size))
			if _, err := file.ReadAt(metaBuf, int64(metaHandle.Offset)); err == nil {
				if len(metaBuf) >= 4 {
					report.MetaStoredCRC = binary.GetUint32(metaBuf[len(metaBuf)-4:])
					report.MetaCompCRC = binary.Checksum(metaBuf[:len(metaBuf)-4])
					report.MetaCRCPass = (report.MetaStoredCRC == report.MetaCompCRC)
				}
				metaMap, err := sstable.DecodeMetaIndexBlock(metaBuf)
				if err == nil {
					report.MetaEntryCnt = len(metaMap)
					if filterHandle, ok := metaMap[filter.FilterMetaKey]; ok {
						report.Filter.Present = true
						report.Filter.Offset = filterHandle.Offset
						report.Filter.Size = filterHandle.Size

						maxFilterSize := uint64(filter.MaxBitsetBytes + filter.FilterBlockTrailerSize)
						if filterHandle.Size <= maxFilterSize && filterHandle.Offset <= math.MaxUint64-filterHandle.Size && filterHandle.Offset+filterHandle.Size <= metaHandle.Offset {
							filterBuf := make([]byte, int(filterHandle.Size))
							if _, err := file.ReadAt(filterBuf, int64(filterHandle.Offset)); err == nil {
								if len(filterBuf) >= 4 {
									report.Filter.StoredCRC = binary.GetUint32(filterBuf[len(filterBuf)-4:])
									report.Filter.ComputedCRC = binary.Checksum(filterBuf[:len(filterBuf)-4])
									report.Filter.CRCPass = (report.Filter.StoredCRC == report.Filter.ComputedCRC)
								}
								bloom, err := filter.DecodeFilterBlock(filterBuf)
								if err == nil {
									report.Filter.BitCount = bloom.BitCount()
									report.Filter.HashCount = bloom.HashCount()
									report.Filter.KeyCount = bloom.KeyCount()
									report.Filter.ByteSize = bloom.ByteSize()
								} else {
									report.Filter.Error = err.Error()
								}
							} else {
								report.Filter.Error = err.Error()
							}
						} else {
							report.Filter.Error = "filter handle exceeds bounds or overlaps metaindex"
						}
					}
				} else {
					report.MetaError = err.Error()
				}
			} else {
				report.MetaError = err.Error()
			}
		} else {
			report.MetaError = "meta-index handle exceeds file bounds"
		}
	}

	// 4. Inspect Data Blocks
	entries := blockIndex.Entries()
	report.DataBlocks = make([]DataBlockInfo, len(entries))

	var globalFirstKey, globalLastKey []byte
	var globalFirstIK, globalLastIK binary.InternalKey
	var previousRecordKey []byte
	allBlocksValid := true

	for i, entry := range entries {
		info := DataBlockInfo{
			Index:  i,
			Offset: entry.Handle.Offset,
			Size:   entry.Handle.Size,
		}

		if entry.Handle.Size > sstable.MaxDataBlockSize {
			info.Error = fmt.Sprintf("block size %d exceeds MaxDataBlockSize %d", entry.Handle.Size, sstable.MaxDataBlockSize)
			report.DataBlocks[i] = info
			allBlocksValid = false
			continue
		}
		if entry.Handle.Offset > math.MaxUint64-entry.Handle.Size || entry.Handle.Offset+entry.Handle.Size > uint64(fileSize)-sstable.FooterSize {
			info.Error = fmt.Sprintf("block handle [%d, %d] exceeds file boundary", entry.Handle.Offset, entry.Handle.Size)
			report.DataBlocks[i] = info
			allBlocksValid = false
			continue
		}

		blockBuf := make([]byte, int(entry.Handle.Size))
		if _, err := file.ReadAt(blockBuf, int64(entry.Handle.Offset)); err != nil {
			info.Error = fmt.Sprintf("read failed: %v", err)
			report.DataBlocks[i] = info
			allBlocksValid = false
			continue
		}

		if len(blockBuf) < 8 {
			info.Error = "data block buffer smaller than 8-byte minimum trailer"
			report.DataBlocks[i] = info
			allBlocksValid = false
			continue
		}

		info.StoredCRC = binary.GetUint32(blockBuf[len(blockBuf)-4:])
		info.ComputedCRC = binary.Checksum(blockBuf[:len(blockBuf)-4])
		info.CRCPass = (info.StoredCRC == info.ComputedCRC)
		if !info.CRCPass {
			info.Error = fmt.Sprintf("CRC mismatch: stored 0x%08x, computed 0x%08x", info.StoredCRC, info.ComputedCRC)
			report.DataBlocks[i] = info
			allBlocksValid = false
			continue
		}

		restartCount := binary.GetUint32(blockBuf[len(blockBuf)-8 : len(blockBuf)-4])
		if restartCount == 0 || restartCount > sstable.MaxRestartCount {
			info.Error = fmt.Sprintf("invalid restart count %d", restartCount)
			report.DataBlocks[i] = info
			allBlocksValid = false
			continue
		}
		info.RestartCount = int(restartCount)

		restartBytes := uint64(restartCount) * 4
		if restartBytes+8 > uint64(len(blockBuf)) {
			info.Error = "restart array exceeds block size"
			report.DataBlocks[i] = info
			allBlocksValid = false
			continue
		}
		entryDataEnd := len(blockBuf) - 8 - int(restartBytes)

		// Parse restart offsets
		info.Restarts = make([]uint32, restartCount)
		restartOffsetsValid := true
		for r := 0; r < int(restartCount); r++ {
			pos := entryDataEnd + r*4
			off := binary.GetUint32(blockBuf[pos : pos+4])
			if r == 0 && off != 0 {
				info.Error = "first restart offset is not zero"
				restartOffsetsValid = false
				break
			}
			if r > 0 && off <= info.Restarts[r-1] {
				info.Error = "restart offsets are not strictly increasing"
				restartOffsetsValid = false
				break
			}
			if uint64(off) >= uint64(entryDataEnd) {
				info.Error = "restart offset exceeds entry data boundary"
				restartOffsetsValid = false
				break
			}
			info.Restarts[r] = off
		}
		if !restartOffsetsValid {
			report.DataBlocks[i] = info
			allBlocksValid = false
			continue
		}

		// Parse records forward
		currOffset := 0
		var reconstructedKey []byte
		var blockFirstKey, blockLastKey []byte
		var blockFirstIK, blockLastIK binary.InternalKey
		recordsCount := 0
		recordsValid := true

		for currOffset < entryDataEnd {
			slice := blockBuf[currOffset:entryDataEnd]

			shared, n1, err := binary.GetVarint64Canonical(slice)
			if err != nil {
				info.Error = fmt.Sprintf("corrupt shared key varint at block offset %d: %v", currOffset, err)
				recordsValid = false
				break
			}
			unshared, n2, err := binary.GetVarint64Canonical(slice[n1:])
			if err != nil {
				info.Error = fmt.Sprintf("corrupt unshared key varint at block offset %d: %v", currOffset, err)
				recordsValid = false
				break
			}
			valLen, n3, err := binary.GetVarint64Canonical(slice[n1+n2:])
			if err != nil {
				info.Error = fmt.Sprintf("corrupt value length varint at block offset %d: %v", currOffset, err)
				recordsValid = false
				break
			}

			if unshared > binary.MaxEncodedInternalKeyLen {
				info.Error = fmt.Sprintf("unshared key length %d exceeds maximum", unshared)
				recordsValid = false
				break
			}
			if valLen > binary.MaxValueLen {
				info.Error = fmt.Sprintf("value length %d exceeds maximum", valLen)
				recordsValid = false
				break
			}

			payloadOffset := n1 + n2 + n3
			totalRecordLen := payloadOffset + int(unshared) + int(valLen)
			if totalRecordLen > len(slice) {
				info.Error = fmt.Sprintf("record length %d overflows remaining block capacity %d", totalRecordLen, len(slice))
				recordsValid = false
				break
			}

			if shared > uint64(len(reconstructedKey)) {
				info.Error = fmt.Sprintf("shared key length %d exceeds previous reconstructed key length %d", shared, len(reconstructedKey))
				recordsValid = false
				break
			}

			keyDelta := slice[payloadOffset : payloadOffset+int(unshared)]
			fullKey := make([]byte, int(shared)+int(unshared))
			copy(fullKey[:shared], reconstructedKey[:shared])
			copy(fullKey[shared:], keyDelta)
			reconstructedKey = fullKey

			ik, err := binary.DecodeInternalKey(reconstructedKey)
			if err != nil {
				info.Error = fmt.Sprintf("invalid internal key at record %d: %v", recordsCount, err)
				recordsValid = false
				break
			}

			// Validate monotonic ordering
			if previousRecordKey != nil {
				prevIK, err := binary.DecodeInternalKey(previousRecordKey)
				if err == nil {
					if binary.CompareInternalKey(prevIK, ik) >= 0 {
						info.Error = fmt.Sprintf("keys out of canonical order at record %d: prev >= curr", recordsCount)
						recordsValid = false
						break
					}
				}
			}

			previousRecordKey = reconstructedKey

			if recordsCount == 0 {
				blockFirstKey = reconstructedKey
				blockFirstIK = ik
			}
			blockLastKey = reconstructedKey
			blockLastIK = ik

			recordsCount++
			currOffset += totalRecordLen
		}

		if !recordsValid {
			report.DataBlocks[i] = info
			allBlocksValid = false
			continue
		}

		info.RecordCount = recordsCount
		info.FirstKey = blockFirstKey
		info.LastKey = blockLastKey
		info.FirstKeyIK = blockFirstIK
		info.LastKeyIK = blockLastIK
		report.DataBlocks[i] = info

		report.TotalRecs += recordsCount
		if globalFirstKey == nil {
			globalFirstKey = blockFirstKey
			globalFirstIK = blockFirstIK
		}
		if blockLastKey != nil {
			globalLastKey = blockLastKey
			globalLastIK = blockLastIK
		}
	}

	report.FirstKey = globalFirstKey
	report.LastKey = globalLastKey
	report.FirstKeyIK = globalFirstIK
	report.LastKeyIK = globalLastIK
	report.HasKeys = (globalFirstKey != nil)

	report.Valid = allBlocksValid
	if !allBlocksValid && report.CorruptionNote == "" {
		for _, b := range report.DataBlocks {
			if b.Error != "" {
				report.CorruptionNote = fmt.Sprintf("data block %d: %s", b.Index, b.Error)
				break
			}
		}
	}

	return nil
}

// PrintReport outputs the formatted forensic inspection report to out.
func PrintReport(r *ForensicReport, verbose bool, out io.Writer) {
	fmt.Fprintf(out, "[SSTable Forensic Report]\n")
	fmt.Fprintf(out, "File Path         : %s\n", r.Path)
	fmt.Fprintf(out, "File Size         : %d bytes\n", r.FileSize)
	if r.Valid {
		fmt.Fprintf(out, "Status            : VALID\n")
	} else {
		fmt.Fprintf(out, "Status            : CORRUPT (%s)\n", r.CorruptionNote)
	}
	fmt.Fprintln(out)

	// Footer Section
	fmt.Fprintf(out, "[Footer]\n")
	if r.MagicValid {
		fmt.Fprintf(out, "Magic Number      : 0x%016x (VALID)\n", r.FooterMagic)
	} else {
		fmt.Fprintf(out, "Magic Number      : 0x%016x (INVALID)\n", r.FooterMagic)
	}
	if r.PaddingValid {
		fmt.Fprintf(out, "Padding           : 8 zero bytes (VALID)\n")
	} else {
		fmt.Fprintf(out, "Padding           : INVALID\n")
	}
	fmt.Fprintf(out, "Index Handle      : Offset=%d, Size=%d bytes\n", r.IndexHandle.Offset, r.IndexHandle.Size)
	fmt.Fprintf(out, "MetaIndex Handle  : Offset=%d, Size=%d bytes\n", r.MetaHandle.Offset, r.MetaHandle.Size)
	fmt.Fprintln(out)

	// Block Index Section
	fmt.Fprintf(out, "[Block Index]\n")
	fmt.Fprintf(out, "Index Block Size  : %d bytes\n", r.IndexBlockSize)
	fmt.Fprintf(out, "Total Data Blocks : %d\n", r.IndexEntryCnt)
	if r.IndexCRCPass {
		fmt.Fprintf(out, "Index CRC32       : 0x%08x (PASS)\n", r.IndexStoredCRC)
	} else {
		fmt.Fprintf(out, "Index CRC32       : stored=0x%08x, computed=0x%08x (FAIL)\n", r.IndexStoredCRC, r.IndexCompCRC)
	}
	fmt.Fprintln(out)

	// Bloom Filter Section
	fmt.Fprintf(out, "[Bloom Filter]\n")
	if r.Filter.Present {
		fmt.Fprintf(out, "Filter Present    : YES\n")
		fmt.Fprintf(out, "Filter Offset     : %d bytes\n", r.Filter.Offset)
		fmt.Fprintf(out, "Filter Size       : %d bytes\n", r.Filter.Size)
		if r.Filter.CRCPass {
			fmt.Fprintf(out, "Filter CRC32      : 0x%08x (PASS)\n", r.Filter.StoredCRC)
		} else {
			fmt.Fprintf(out, "Filter CRC32      : stored=0x%08x, computed=0x%08x (FAIL)\n", r.Filter.StoredCRC, r.Filter.ComputedCRC)
		}
		fmt.Fprintf(out, "Bit Count         : %d bits\n", r.Filter.BitCount)
		fmt.Fprintf(out, "Hash Functions (k): %d\n", r.Filter.HashCount)
		fmt.Fprintf(out, "Estimated Keys    : %d\n", r.Filter.KeyCount)
		fmt.Fprintf(out, "Bitset Payload    : %d bytes\n", r.Filter.ByteSize)
		if r.Filter.Error != "" {
			fmt.Fprintf(out, "Filter Error      : %s\n", r.Filter.Error)
		}
	} else {
		fmt.Fprintf(out, "Filter Present    : NO\n")
	}
	fmt.Fprintln(out)

	// Data Blocks Summary
	fmt.Fprintf(out, "[Data Blocks]\n")
	fmt.Fprintf(out, "Data Block Count  : %d\n", len(r.DataBlocks))
	fmt.Fprintf(out, "Total Records     : %d\n", r.TotalRecs)
	for _, b := range r.DataBlocks {
		crcStatus := "PASS"
		if !b.CRCPass {
			crcStatus = fmt.Sprintf("FAIL (stored=0x%08x, comp=0x%08x)", b.StoredCRC, b.ComputedCRC)
		}
		if b.Error != "" {
			fmt.Fprintf(out, "  Block %-4d: Offset=%-8d Size=%-6d CRC=%s ERROR: %s\n", b.Index, b.Offset, b.Size, crcStatus, b.Error)
		} else {
			fmt.Fprintf(out, "  Block %-4d: Offset=%-8d Size=%-6d Records=%-5d Restarts=%-3d CRC=%s\n", b.Index, b.Offset, b.Size, b.RecordCount, b.RestartCount, crcStatus)
			if verbose {
				fmt.Fprintf(out, "              First: %s\n", FormatBytes(b.FirstKeyIK.UserKey))
				fmt.Fprintf(out, "              Last : %s\n", FormatBytes(b.LastKeyIK.UserKey))
				fmt.Fprintf(out, "              Restarts: %v\n", b.Restarts)
			}
		}
	}
	fmt.Fprintln(out)

	// Key Range
	fmt.Fprintf(out, "[Key Range]\n")
	if r.HasKeys {
		fmt.Fprintf(out, "First User Key    : %s (SeqNum=%d, Type=%s)\n", FormatBytes(r.FirstKeyIK.UserKey), r.FirstKeyIK.SeqNum, r.FirstKeyIK.OpType)
		fmt.Fprintf(out, "Last User Key     : %s (SeqNum=%d, Type=%s)\n", FormatBytes(r.LastKeyIK.UserKey), r.LastKeyIK.SeqNum, r.LastKeyIK.OpType)
	} else {
		fmt.Fprintf(out, "First User Key    : none\n")
		fmt.Fprintf(out, "Last User Key     : none\n")
	}
}

// runInspectSSTable handles CLI argument parsing and execution for inspect-sstable.
func runInspectSSTable(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("inspect-sstable", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		flagVerbose      bool
		flagHelp         bool
		flagHelpShort    bool
		flagVersion      bool
		flagVersionShort bool
	)

	fs.BoolVar(&flagVerbose, "v", false, "Display verbose per-block record metadata and restart offsets")
	fs.BoolVar(&flagVerbose, "verbose", false, "Display verbose per-block record metadata and restart offsets")
	fs.BoolVar(&flagHelp, "help", false, "Display usage instructions and exit")
	fs.BoolVar(&flagHelpShort, "h", false, "Display usage instructions and exit")
	fs.BoolVar(&flagVersion, "version", false, "Display version information and exit")
	fs.BoolVar(&flagVersionShort, "V", false, "Display version information and exit")

	fs.Usage = func() {
		fmt.Fprintf(stdout, "Lattice SSTable Forensic Inspection Tool (%s)\n\n", Version)
		fmt.Fprintf(stdout, "Usage:\n")
		fmt.Fprintf(stdout, "  lattice inspect-sstable [flags] <sstable-path>\n\n")
		fmt.Fprintf(stdout, "Flags:\n")
		fmt.Fprintf(stdout, "  -v, --verbose    Print detailed per-block key ranges and restart offsets\n")
		fmt.Fprintf(stdout, "  -h, --help       Show usage instructions\n")
		fmt.Fprintf(stdout, "  -V, --version    Show version information\n")
	}

	if err := fs.Parse(args); err != nil {
		return ExitInspectUsageError
	}

	if flagHelp || flagHelpShort {
		fs.Usage()
		return ExitInspectSuccess
	}

	if flagVersion || flagVersionShort {
		fmt.Fprintf(stdout, "lattice inspect-sstable %s\n", Version)
		return ExitInspectSuccess
	}

	remaining := fs.Args()
	if len(remaining) == 0 {
		fmt.Fprintf(stderr, "error: missing required SSTable file path\n\n")
		fs.Usage()
		return ExitInspectUsageError
	}
	if len(remaining) > 1 {
		fmt.Fprintf(stderr, "error: inspect-sstable takes exactly one file path, got %d\n", len(remaining))
		return ExitInspectUsageError
	}

	targetPath := remaining[0]

	// Read-only inspection
	var report ForensicReport
	if err := InspectSSTable(targetPath, &report); err != nil {
		fmt.Fprintf(stderr, "error: failed to access SSTable %q: %v\n", targetPath, err)
		return ExitInspectFileError
	}

	PrintReport(&report, flagVerbose, stdout)

	if !report.Valid {
		return ExitInspectCorruptError
	}

	return ExitInspectSuccess
}
