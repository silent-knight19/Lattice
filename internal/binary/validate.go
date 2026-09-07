package binary

import (
	"math"

	"github.com/silent-knight19/lattice/internal/errors"
)

const (
	// MinKeyLen is the minimum legal byte length for a user key (1 byte).
	MinKeyLen = 1

	// MaxKeyLen is the maximum legal byte length for a user key (65,535 bytes / 64 KB - 1).
	MaxKeyLen = 65535

	// MaxKeyBytes is an alias for MaxKeyLen.
	MaxKeyBytes = MaxKeyLen

	// MinValueLen is the minimum legal byte length for a value (0 bytes).
	// Zero-length values are valid valueless markers (e.g., in tombstone deletions or sets).
	MinValueLen = 0

	// MaxValueLen is the maximum legal byte length for a value (4,194,304 bytes / 4 MiB).
	MaxValueLen = 4 * 1024 * 1024

	// MaxValueBytes is an alias for MaxValueLen.
	MaxValueBytes = MaxValueLen
)

// ValidateKey verifies that key satisfies storage boundary constraints:
// 1 <= len(key) <= 65,535 bytes.
//
// Properties:
//   - O(1) time complexity: checks only slice header length without scanning or copying payload.
//   - Zero heap allocations on valid keys or empty keys.
//   - Non-mutating: does not modify the provided slice.
//   - Binary-safe: enforces byte length regardless of content (UTF-8, binary, null bytes).
//
// Returns:
//   - nil if key is valid.
//   - errors.ErrEmptyKey if len(key) == 0 (including nil).
//   - *errors.KeyTooLargeError if len(key) > MaxKeyLen (matches errors.ErrKeyTooLarge via errors.Is).
func ValidateKey(key []byte) error {
	l := len(key)
	if l == 0 {
		return errors.ErrEmptyKey
	}
	if l > MaxKeyLen {
		return &errors.KeyTooLargeError{
			KeySize: safeUint32(l),
			MaxSize: MaxKeyLen,
		}
	}
	return nil
}

// ValidateValue verifies that val satisfies storage boundary constraints:
// 0 <= len(val) <= 4,194,304 bytes (4 MiB).
//
// Properties:
//   - O(1) time complexity: checks only slice header length without scanning or copying payload.
//   - Zero heap allocations on valid values (including empty/nil).
//   - Non-mutating: does not modify the provided slice.
//   - Binary-safe: zero-length values are legal markers.
//
// Returns:
//   - nil if val is valid (including nil and empty slices).
//   - *errors.ValueTooLargeError if len(val) > MaxValueLen (matches errors.ErrValueTooLarge via errors.Is).
func ValidateValue(val []byte) error {
	l := len(val)
	if l > MaxValueLen {
		return &errors.ValueTooLargeError{
			ValueSize: safeUint32(l),
			MaxSize:   MaxValueLen,
		}
	}
	return nil
}

func safeUint32(n int) uint32 {
	if n < 0 {
		return 0
	}
	if uint64(n) > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(n)
}
