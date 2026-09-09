package binary

import (
	"math/bits"

	"github.com/silent-knight19/lattice/internal/errors"
)

// MaxVarintLen64 is the maximum number of bytes required to encode a uint64 varint.
const MaxVarintLen64 = 10

// VarintLen returns the exact number of bytes required to encode v as a 7-bit varint.
// It returns a value between 1 and 10.
func VarintLen(v uint64) int {
	if v == 0 {
		return 1
	}
	return (bits.Len64(v) + 6) / 7
}

// PutVarint64 encodes a uint64 into buf using unsigned 7-bit variable-length encoding.
// It returns the number of bytes written (1 <= n <= 10).
//
// Encoding format:
//   - Each byte carries 7 payload bits and 1 continuation bit (0x80).
//   - Lower-order 7-bit groups are serialized first (Little-Endian group ordering).
//   - The continuation bit (0x80) indicates whether subsequent bytes follow.
//   - The representation is strictly canonical (minimal bytes needed).
//
// Buffer contract:
//   - Requires len(buf) >= VarintLen(v).
//   - If len(buf) < VarintLen(v) or buf == nil, it panics with a runtime bounds error.
//   - Bounds check is performed eagerly before any bytes are written,
//     preventing partial or torn writes to undersized buffers.
//   - Trailing bytes beyond the encoded length are completely untouched.
//   - Zero heap allocations.
func PutVarint64(buf []byte, v uint64) int {
	needed := VarintLen(v)
	_ = buf[needed-1] // Early bounds check: panics before partial write if len(buf) < needed

	i := 0
	for v >= 0x80 {
		buf[i] = byte(v) | 0x80
		v >>= 7
		i++
	}
	buf[i] = byte(v)
	return i + 1
}

// GetVarint64 decodes a uint64 from buf using unsigned 7-bit variable-length encoding.
// It returns the decoded value, the number of bytes consumed, and an error if decoding failed.
//
// Return contract:
//   - On success: returns (value, bytesConsumed, nil), where 1 <= bytesConsumed <= 10.
//   - On truncation: returns (0, 0, errors.ErrVarintTruncated) when buf is empty or
//     terminates while the continuation bit (0x80) is still set.
//   - On overflow: returns (0, 0, errors.ErrVarintOverflow) when the varint exceeds
//     10 bytes, or the 10th byte contains invalid payload bits (> 1) exceeding 64 bits.
//   - Safe against malicious inputs: execution is strictly bounded to at most 10 iterations,
//     guaranteeing immunity against infinite loop DoS attacks.
//   - Trailing bytes beyond the encoded varint are untouched and ignored.
//   - Zero heap allocations.
func GetVarint64(buf []byte) (uint64, int, error) {
	if len(buf) == 0 {
		return 0, 0, errors.ErrVarintTruncated
	}

	// Fast path: single-byte canonical varint (values 0..127).
	if buf[0] < 0x80 {
		return uint64(buf[0]), 1, nil
	}

	var val uint64
	var shift uint

	for i := 0; i < len(buf); i++ {
		b := buf[i]
		if i == 9 {
			// The 10th byte can only provide 1 bit (the 64th bit, since 9 * 7 = 63).
			// If b > 1:
			//   - If b & 0x80 != 0: continuation bit is set, requiring an illegal 11th byte.
			//   - If b & 0x7E != 0: payload bits 1..6 are set, representing values >= 2^64.
			// Both cases represent an illegal overflow of uint64.
			if b > 1 {
				return 0, 0, errors.ErrVarintOverflow
			}
			val |= uint64(b) << shift
			return val, 10, nil
		}

		val |= uint64(b&0x7F) << shift
		if b < 0x80 {
			return val, i + 1, nil
		}
		shift += 7
	}

	// Reached end of slice while continuation bit was still set.
	return 0, 0, errors.ErrVarintTruncated
}

// GetVarint64Canonical decodes a uint64 from buf using unsigned 7-bit variable-length encoding,
// strictly enforcing canonical minimal-byte representations.
//
// In addition to standard GetVarint64 validation:
//   - Rejects non-canonical (overlong) encodings with errors.ErrVarintNonCanonical
//     where a value was encoded using more bytes than mathematically required
//     (e.g., [0x80, 0x00] for 0, or [0x81, 0x00] for 1).
//   - Safe against parser ambiguity and cryptographic malleability attacks.
//   - Zero heap allocations.
func GetVarint64Canonical(buf []byte) (uint64, int, error) {
	val, n, err := GetVarint64(buf)
	if err != nil {
		return 0, 0, err
	}
	if VarintLen(val) != n {
		return 0, 0, errors.ErrVarintNonCanonical
	}
	return val, n, nil
}
