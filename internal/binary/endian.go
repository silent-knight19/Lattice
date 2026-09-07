package binary

// PutUint16 encodes a uint16 into buf using strict Big-Endian (network byte order).
// It writes exactly 2 bytes: buf[0] = MSB, buf[1] = LSB.
//
// Buffer contract:
//   - Requires len(buf) >= 2.
//   - If len(buf) < 2 or buf == nil, it panics with a runtime bounds error.
//   - Bounds check is performed eagerly at index 1 before any bytes are written,
//     preventing partial or torn writes to undersized buffers.
//   - If len(buf) > 2, only buf[0..1] are modified; trailing bytes are untouched.
//   - Zero heap allocations.
func PutUint16(buf []byte, v uint16) {
	_ = buf[1] // Early bounds check: panics before partial write if len(buf) < 2
	buf[0] = byte(v >> 8)
	buf[1] = byte(v)
}

// GetUint16 decodes a uint16 from buf using strict Big-Endian (network byte order).
// It reads exactly 2 bytes: buf[0] as MSB, buf[1] as LSB.
//
// Buffer contract:
//   - Requires len(buf) >= 2.
//   - If len(buf) < 2 or buf == nil, it panics with a runtime bounds error.
//   - If len(buf) > 2, only buf[0..1] are read; trailing bytes are ignored.
//   - Zero heap allocations.
func GetUint16(buf []byte) uint16 {
	_ = buf[1] // Early bounds check: panics if len(buf) < 2
	return uint16(buf[1]) | uint16(buf[0])<<8
}

// PutUint32 encodes a uint32 into buf using strict Big-Endian (network byte order).
// It writes exactly 4 bytes: buf[0] = MSB, buf[3] = LSB.
//
// Buffer contract:
//   - Requires len(buf) >= 4.
//   - If len(buf) < 4 or buf == nil, it panics with a runtime bounds error.
//   - Bounds check is performed eagerly at index 3 before any bytes are written,
//     preventing partial or torn writes to undersized buffers.
//   - If len(buf) > 4, only buf[0..3] are modified; trailing bytes are untouched.
//   - Zero heap allocations.
func PutUint32(buf []byte, v uint32) {
	_ = buf[3] // Early bounds check: panics before partial write if len(buf) < 4
	buf[0] = byte(v >> 24)
	buf[1] = byte(v >> 16)
	buf[2] = byte(v >> 8)
	buf[3] = byte(v)
}

// GetUint32 decodes a uint32 from buf using strict Big-Endian (network byte order).
// It reads exactly 4 bytes: buf[0] as MSB, buf[3] as LSB.
//
// Buffer contract:
//   - Requires len(buf) >= 4.
//   - If len(buf) < 4 or buf == nil, it panics with a runtime bounds error.
//   - If len(buf) > 4, only buf[0..3] are read; trailing bytes are ignored.
//   - Zero heap allocations.
func GetUint32(buf []byte) uint32 {
	_ = buf[3] // Early bounds check: panics if len(buf) < 4
	return uint32(buf[3]) | uint32(buf[2])<<8 | uint32(buf[1])<<16 | uint32(buf[0])<<24
}

// PutUint64 encodes a uint64 into buf using strict Big-Endian (network byte order).
// It writes exactly 8 bytes: buf[0] = MSB, buf[7] = LSB.
//
// Buffer contract:
//   - Requires len(buf) >= 8.
//   - If len(buf) < 8 or buf == nil, it panics with a runtime bounds error.
//   - Bounds check is performed eagerly at index 7 before any bytes are written,
//     preventing partial or torn writes to undersized buffers.
//   - If len(buf) > 8, only buf[0..7] are modified; trailing bytes are untouched.
//   - Zero heap allocations.
func PutUint64(buf []byte, v uint64) {
	_ = buf[7] // Early bounds check: panics before partial write if len(buf) < 8
	buf[0] = byte(v >> 56)
	buf[1] = byte(v >> 48)
	buf[2] = byte(v >> 40)
	buf[3] = byte(v >> 32)
	buf[4] = byte(v >> 24)
	buf[5] = byte(v >> 16)
	buf[6] = byte(v >> 8)
	buf[7] = byte(v)
}

// GetUint64 decodes a uint64 from buf using strict Big-Endian (network byte order).
// It reads exactly 8 bytes: buf[0] as MSB, buf[7] as LSB.
//
// Buffer contract:
//   - Requires len(buf) >= 8.
//   - If len(buf) < 8 or buf == nil, it panics with a runtime bounds error.
//   - If len(buf) > 8, only buf[0..7] are read; trailing bytes are ignored.
//   - Zero heap allocations.
func GetUint64(buf []byte) uint64 {
	_ = buf[7] // Early bounds check: panics if len(buf) < 8
	return uint64(buf[7]) | uint64(buf[6])<<8 | uint64(buf[5])<<16 | uint64(buf[4])<<24 |
		uint64(buf[3])<<32 | uint64(buf[2])<<40 | uint64(buf[1])<<48 | uint64(buf[0])<<56
}
