package version

import (
	stdErrors "errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
)

// ManifestReadResult describes the outcome of a MANIFEST recovery scan.
type ManifestReadResult struct {
	Path            string
	Edits           []*VersionEdit
	ValidRecords    int
	RecoveredOffset int64
	FileSize        int64
	Truncated       bool
	TruncatedBytes  int64
}

// ReadManifest strictly reads and verifies all MANIFEST records.
// Fails closed on any truncation or CRC corruption (no truncation performed).
func ReadManifest(path string) ([]*VersionEdit, error) {
	res, err := scanManifest(path, false)
	if err != nil {
		return nil, err
	}
	return res.Edits, nil
}

// RecoverManifest preserves the CRC-verified prefix and truncates a torn
// tail at EOF (header/payload truncation only). Mid-log CRC mismatch or
// undecodable VersionEdit fails closed without mutation — mirroring
// wal.RecoverSegment semantics.
func RecoverManifest(path string) (ManifestReadResult, error) {
	return scanManifest(path, true)
}

func scanManifest(path string, allowTruncate bool) (ManifestReadResult, error) {
	var res ManifestReadResult
	if path == "" {
		return res, fmt.Errorf("%w: path cannot be empty", os.ErrInvalid)
	}
	cleanPath := filepath.Clean(path)
	res.Path = cleanPath

	info, err := os.Lstat(cleanPath)
	if err != nil {
		return res, fmt.Errorf("manifest: failed to inspect path %s: %w", cleanPath, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return res, fmt.Errorf("manifest: cannot read symlink %s: %w", cleanPath, os.ErrInvalid)
	}
	if info.IsDir() {
		return res, &errors.NotADirectoryError{Path: cleanPath, Mode: info.Mode()}
	}
	if !info.Mode().IsRegular() {
		return res, fmt.Errorf("manifest: path %s is not a regular file (mode: %s): %w", cleanPath, info.Mode(), os.ErrInvalid)
	}

	openFlags := os.O_RDONLY
	var f *os.File
	if allowTruncate {
		f, err = os.OpenFile(cleanPath, os.O_RDWR, 0)
	} else {
		f, err = os.OpenFile(cleanPath, os.O_RDONLY, 0)
		_ = openFlags
	}
	if err != nil {
		return res, fmt.Errorf("manifest: failed to open file %s: %w", cleanPath, err)
	}
	defer func() { _ = f.Close() }()

	finfo, statErr := f.Stat()
	if statErr != nil {
		return res, fmt.Errorf("manifest: failed to stat opened file %s: %w", cleanPath, statErr)
	}
	if !finfo.Mode().IsRegular() {
		return res, fmt.Errorf("manifest: path %s is not a regular file (mode: %s): %w", cleanPath, finfo.Mode(), os.ErrInvalid)
	}
	postInfo, lstatErr := os.Lstat(cleanPath)
	if lstatErr != nil {
		return res, fmt.Errorf("manifest: failed to lstat file %s: %w", cleanPath, lstatErr)
	}
	if !os.SameFile(finfo, postInfo) || !os.SameFile(finfo, info) {
		return res, fmt.Errorf("manifest: file %s was replaced during open: %w", cleanPath, os.ErrInvalid)
	}

	res.FileSize = finfo.Size()
	var offset int64
	for {
		payload, recordLen, decErr := decodeOneManifestRecord(f, offset)
		if decErr == nil {
			edit, err := DecodeVersionEdit(payload)
			if err != nil {
				// Complete framing but undecodable edit = mid-log corruption, fail closed.
				res.RecoveredOffset = offset
				return res, fmt.Errorf("manifest: record at offset %d undecodable: %w", offset, err)
			}
			res.Edits = append(res.Edits, edit)
			res.ValidRecords++
			offset += recordLen
			continue
		}
		if stdErrors.Is(decErr, io.EOF) {
			break
		}
		if stdErrors.Is(decErr, errors.ErrManifestHeaderTruncated) || stdErrors.Is(decErr, errors.ErrManifestPayloadTruncated) {
			// Torn tail at EOF: recoverable via truncation only.
			res.RecoveredOffset = offset
			if !allowTruncate {
				return res, decErr
			}
			if offset >= res.FileSize {
				return res, fmt.Errorf("manifest: torn tail indicated but offset %d >= file size %d", offset, res.FileSize)
			}
			if err := f.Truncate(offset); err != nil {
				return res, fmt.Errorf("manifest: failed to truncate torn tail at offset %d: %w", offset, err)
			}
			if err := f.Sync(); err != nil {
				res.Truncated = true
				res.TruncatedBytes = res.FileSize - offset
				return res, fmt.Errorf("manifest: failed to sync truncated file %s: %w", cleanPath, err)
			}
			res.Truncated = true
			res.TruncatedBytes = res.FileSize - offset
			res.FileSize = offset
			res.RecoveredOffset = offset
			return res, nil
		}
		// CRC mismatch or other corruption: fail closed, no truncation.
		res.RecoveredOffset = offset
		return res, decErr
	}

	res.RecoveredOffset = offset
	return res, nil
}

// decodeOneManifestRecord reads a single framed record at absolute offset.
// Returns (payload, totalRecordLen, nil) on success; (nil, 0, io.EOF) on clean
// EOF; (nil, 0, ErrManifestHeaderTruncated/PayloadTruncated) on torn tail;
// (nil, 0, ChecksumMismatchError/ManifestCorruptedError) on corruption.
func decodeOneManifestRecord(f *os.File, offset int64) ([]byte, int64, error) {
	var header [ManifestHeaderSize]byte
	n, err := readFullAt(f, header[:], offset)
	if err != nil {
		if stdErrors.Is(err, io.EOF) && n == 0 {
			return nil, 0, io.EOF
		}
		return nil, 0, errors.ErrManifestHeaderTruncated
	}
	expectedCRC := binary.GetUint32(header[0:4])
	payloadLen := binary.GetUint32(header[4:8])
	if payloadLen > MaxVersionEditBytes {
		return nil, 0, &errors.ManifestCorruptedError{
			Offset: offset,
			Reason: fmt.Sprintf("payload length %d exceeds maximum %d", payloadLen, MaxVersionEditBytes),
		}
	}
	payload := make([]byte, payloadLen)
	if _, err := readFullAt(f, payload, offset+ManifestHeaderSize); err != nil {
		return nil, 0, errors.ErrManifestPayloadTruncated
	}
	// CRC over [PayloadLength || Payload]
	crc := crc32.Update(0, crc32.IEEETable, header[4:ManifestHeaderSize])
	crc = crc32.Update(crc, crc32.IEEETable, payload)
	if crc != expectedCRC {
		return nil, 0, &errors.ChecksumMismatchError{
			Offset:   offset,
			Expected: expectedCRC,
			Actual:   crc,
		}
	}
	return payload, int64(ManifestHeaderSize) + int64(payloadLen), nil
}

func readFullAt(f *os.File, buf []byte, off int64) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := f.ReadAt(buf[total:], off+int64(total))
		total += n
		if err != nil {
			if total == len(buf) {
				return total, nil
			}
			if stdErrors.Is(err, io.EOF) {
				if total == 0 {
					return 0, io.EOF
				}
				return total, io.ErrUnexpectedEOF
			}
			return total, err
		}
		if n == 0 {
			if total == 0 {
				return 0, io.EOF
			}
			return total, io.ErrUnexpectedEOF
		}
	}
	return total, nil
}
