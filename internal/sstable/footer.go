package sstable

import (
	stdErrors "errors"
	"fmt"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
)

const (
	// FooterSize is the exact length in bytes of the fixed SSTable file footer.
	// 16 bytes (MetaIndexHandle) + 16 bytes (IndexHandle) + 8 bytes (Padding) + 8 bytes (Magic) = 48 bytes.
	FooterSize = 48

	// FormatVersion1 is the initial SSTable physical format version.
	FormatVersion1 uint8 = 1

	// CurrentFormatVersion represents the active SSTable physical format version.
	CurrentFormatVersion = FormatVersion1

	// FooterMagicBase is the authoritative 56-bit Big-Endian magic prefix: 0x4C41545453535400 ("LATT_SST_\x00").
	FooterMagicBase uint64 = 0x4C41545453535400

	// FooterMagic is the authoritative 64-bit Big-Endian magic number anchoring every
	// Lattice SSTable file: FooterMagicBase | uint64(CurrentFormatVersion) = 0x4C41545453535401 ("LATT_SST_1").
	FooterMagic uint64 = FooterMagicBase | uint64(CurrentFormatVersion)
)

// ErrUnsupportedFormatVersion indicates that an SSTable footer specifies an unsupported format version.
var ErrUnsupportedFormatVersion = stdErrors.New("sstable: unsupported format version")

// UnsupportedFormatVersionError provides structured context when an SSTable footer specifies an unknown format version.
// It matches ErrUnsupportedFormatVersion, ErrInvalidFooter, and ErrInvalidFooterMagic when interrogated with errors.Is().
type UnsupportedFormatVersionError struct {
	Version  uint8
	Expected uint8
}

func (e *UnsupportedFormatVersionError) Error() string {
	if e == nil {
		return ErrUnsupportedFormatVersion.Error()
	}
	return fmt.Sprintf("sstable: unsupported format version %d (current: %d)", e.Version, e.Expected)
}

// Is reports whether this error matches target sentinels.
func (e *UnsupportedFormatVersionError) Is(target error) bool {
	return target == ErrUnsupportedFormatVersion ||
		target == errors.ErrInvalidFooter ||
		target == errors.ErrInvalidFooterMagic
}

// As supports unboxing into *errors.InvalidFooterMagicError for backward compatibility.
func (e *UnsupportedFormatVersionError) As(target any) bool {
	if ptr, ok := target.(**errors.InvalidFooterMagicError); ok {
		*ptr = &errors.InvalidFooterMagicError{
			Expected: FooterMagic,
			Actual:   FooterMagicBase | uint64(e.Version),
		}
		return true
	}
	return false
}

// FormatVersionFromMagic extracts the format version from an SSTable footer magic number.
// Returns an *errors.InvalidFooterMagicError if the magic base does not match FooterMagicBase,
// or an *UnsupportedFormatVersionError if the version is unrecognized.
func FormatVersionFromMagic(magic uint64) (uint8, error) {
	if (magic &^ 0xFF) != FooterMagicBase {
		return 0, &errors.InvalidFooterMagicError{
			Expected: FooterMagic,
			Actual:   magic,
		}
	}
	version := uint8(magic & 0xFF)
	if version != CurrentFormatVersion {
		return version, &UnsupportedFormatVersionError{
			Version:  version,
			Expected: CurrentFormatVersion,
		}
	}
	return version, nil
}

// Footer represents the fixed 48-byte trailer anchored at the exact physical end of every
// SSTable file (file_size - 48 bytes).
//
// On-Disk Binary Layout (48 Bytes Fixed):
//
//	+-----------------------------------+-----------------------------------+
//	| MetaIndex Handle Offset (8B)      | MetaIndex Handle Size (8B)        |
//	+-----------------------------------+-----------------------------------+
//	| Index Handle Offset (8B)          | Index Handle Size (8B)            |
//	+-----------------------------------+-----------------------------------+
//	| Padding Bytes (8B, All Zero)      | Magic: 0x4C41545453535401 (8B)    |
//	+-----------------------------------+-----------------------------------+
//
// Byte Offsets:
//   - 00..15 (16B): MetaIndexHandle (8B Offset Big-Endian, 8B Size Big-Endian)
//   - 16..31 (16B): IndexHandle (8B Offset Big-Endian, 8B Size Big-Endian)
//   - 32..39 (08B): Padding (strictly 8 zero bytes for canonical framing)
//   - 40..47 (08B): Magic Number (0x4C41545453535401, Big-Endian uint64)
type Footer struct {
	MetaIndexHandle BlockHandle
	IndexHandle     BlockHandle
}

// Encode serializes the Footer into an exact 48-byte array in strict Big-Endian byte order.
// Executes with zero heap allocations.
func (f Footer) Encode() [FooterSize]byte {
	var buf [FooterSize]byte

	// 0..15: MetaIndexHandle (8B Offset, 8B Size)
	binary.PutUint64(buf[0:8], f.MetaIndexHandle.Offset)
	binary.PutUint64(buf[8:16], f.MetaIndexHandle.Size)

	// 16..31: IndexHandle (8B Offset, 8B Size)
	binary.PutUint64(buf[16:24], f.IndexHandle.Offset)
	binary.PutUint64(buf[24:32], f.IndexHandle.Size)

	// 32..39: Padding (strictly 8 zero bytes, guaranteed by Go zero-initialization of buf)

	// 40..47: Magic Number (0x4C41545453535401)
	binary.PutUint64(buf[40:48], FooterMagic)

	return buf
}

// AppendTo serializes the Footer into dst using 48 Big-Endian bytes and returns the extended slice.
func (f Footer) AppendTo(dst []byte) []byte {
	encoded := f.Encode()
	return append(dst, encoded[:]...)
}

// Decode decodes and validates a 48-byte serialized SSTable footer into f.
//
// Validation Contract:
//   - Requires len(src) == FooterSize (48 bytes).
//   - If len(src) < 48: returns errors.ErrFooterTruncated.
//   - If len(src) > 48: returns errors.ErrInvalidFooterSize.
//   - Verifies that magic matches FooterMagic (0x4C41545453535401). Returns errors.ErrInvalidFooterMagic on mismatch.
//   - Verifies that all 8 padding bytes are zero (0x00). Returns errors.ErrInvalidFooterPadding on non-zero padding.
//   - Decodes and validates MetaIndexHandle and IndexHandle via DecodeBlockHandle (Size > 0, no uint64 overflow).
func (f *Footer) Decode(src []byte) error {
	if f == nil {
		return errors.ErrNilReceiver
	}
	if len(src) < FooterSize {
		return &errors.InvalidFooterSizeError{
			Expected: FooterSize,
			Actual:   len(src),
		}
	}
	if len(src) > FooterSize {
		return &errors.InvalidFooterSizeError{
			Expected: FooterSize,
			Actual:   len(src),
		}
	}

	// 1. Verify Magic Number (bytes 40..47) and Format Version
	magic := binary.GetUint64(src[40:48])
	if _, err := FormatVersionFromMagic(magic); err != nil {
		return err
	}

	// 2. Verify Padding (bytes 32..39 must be all zero)
	var padding [8]byte
	copy(padding[:], src[32:40])
	if padding != [8]byte{} {
		return &errors.InvalidFooterPaddingError{
			Padding: padding,
		}
	}

	// 3. Decode MetaIndexHandle (bytes 0..15)
	metaHandle, err := DecodeBlockHandle(src[0:16])
	if err != nil {
		return err
	}

	// 4. Decode IndexHandle (bytes 16..31)
	indexHandle, err := DecodeBlockHandle(src[16:32])
	if err != nil {
		return err
	}

	f.MetaIndexHandle = metaHandle
	f.IndexHandle = indexHandle
	return nil
}

// DecodeFooter parses and validates a 48-byte serialized SSTable footer from src.
func DecodeFooter(src []byte) (Footer, error) {
	var f Footer
	if err := f.Decode(src); err != nil {
		return Footer{}, err
	}
	return f, nil
}

// Validate verifies that both the MetaIndexHandle and IndexHandle satisfy block handle
// integrity constraints (Size > 0 and Offset + Size does not overflow uint64).
func (f Footer) Validate() error {
	if err := f.MetaIndexHandle.Validate(); err != nil {
		return err
	}
	if err := f.IndexHandle.Validate(); err != nil {
		return err
	}
	return nil
}

// ValidateAgainstFileSize checks whether the footer handles fall completely within the physical
// file boundary without overlapping the 48-byte footer anchored at the end of the file.
//
// Invariant: For a file of size fileSize, both MetaIndexHandle and IndexHandle must span within
// [0, fileSize - FooterSize).
func (f Footer) ValidateAgainstFileSize(fileSize int64) error {
	if fileSize < int64(FooterSize) {
		return &errors.InvalidBlockHandleError{
			Reason: "physical file size smaller than 48-byte footer",
		}
	}
	if err := f.Validate(); err != nil {
		return err
	}

	limit := uint64(fileSize) - FooterSize
	if f.MetaIndexHandle.Size > MaxIndexBlockSize {
		return &errors.InvalidBlockHandleError{
			Offset: f.MetaIndexHandle.Offset,
			Size:   f.MetaIndexHandle.Size,
			Reason: "metaindex handle size exceeds MaxIndexBlockSize",
		}
	}
	if f.IndexHandle.Size > MaxIndexBlockSize {
		return &errors.InvalidBlockHandleError{
			Offset: f.IndexHandle.Offset,
			Size:   f.IndexHandle.Size,
			Reason: "index handle size exceeds MaxIndexBlockSize",
		}
	}
	if f.MetaIndexHandle.Offset+f.MetaIndexHandle.Size > limit {
		return &errors.InvalidBlockHandleError{
			Offset: f.MetaIndexHandle.Offset,
			Size:   f.MetaIndexHandle.Size,
			Reason: "metaindex handle overlaps or exceeds footer boundary",
		}
	}
	if f.IndexHandle.Offset+f.IndexHandle.Size > limit {
		return &errors.InvalidBlockHandleError{
			Offset: f.IndexHandle.Offset,
			Size:   f.IndexHandle.Size,
			Reason: "index handle overlaps or exceeds footer boundary",
		}
	}
	return nil
}
