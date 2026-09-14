package sstable

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
)

func FuzzTableIterator(f *testing.F) {
	// 1. Seed with an empty SSTable
	dir := f.TempDir()
	emptyPath := filepath.Join(dir, "empty.sst")
	wEmpty, err := NewTableWriter(emptyPath, DefaultTableWriterOptions())
	if err == nil {
		if _, err := wEmpty.Finish(); err == nil {
			if emptyBytes, err := os.ReadFile(emptyPath); err == nil { // #nosec G304 -- test file path
				f.Add(emptyBytes)
			}
		}
	}

	// 2. Seed with a small valid SSTable
	smallPath := filepath.Join(dir, "small.sst")
	wSmall, err := NewTableWriter(smallPath, DefaultTableWriterOptions())
	if err == nil {
		_ = wSmall.Add(binary.InternalKey{UserKey: []byte("k1"), SeqNum: 10, OpType: binary.OpTypePut}, []byte("v1"))
		_ = wSmall.Add(binary.InternalKey{UserKey: []byte("k2"), SeqNum: 20, OpType: binary.OpTypePut}, []byte("v2"))
		_ = wSmall.Add(binary.InternalKey{UserKey: []byte("k3"), SeqNum: 30, OpType: binary.OpTypeDelete}, nil)
		if _, err := wSmall.Finish(); err == nil {
			if smallBytes, err := os.ReadFile(smallPath); err == nil { // #nosec G304 -- test file path
				f.Add(smallBytes)
			}
		}
	}

	// 3. Seed with truncated bytes
	f.Add([]byte{})
	f.Add([]byte{0x00, 0x01, 0x02, 0x03})
	f.Add(make([]byte, 48)) // minimum footer size

	f.Fuzz(func(t *testing.T, sstableBytes []byte) {
		if len(sstableBytes) > 1024*1024 { // cap fuzzed input to 1MB to avoid resource starvation
			return
		}

		tmpDir := t.TempDir()
		testFile := filepath.Join(tmpDir, "fuzz.sst")
		if err := os.WriteFile(testFile, sstableBytes, 0600); err != nil {
			return
		}

		it, err := OpenTableIterator(testFile)
		if err != nil {
			// Fail-closed rejection is safe and expected
			return
		}
		defer func() { _ = it.Close() }()

		const maxSteps = 1000
		steps := 0
		var prevKey *binary.InternalKey

		for it.Next() {
			steps++
			if steps > maxSteps {
				t.Fatalf("iterator exceeded max steps without terminating (infinite loop)")
			}

			if !it.Valid() {
				t.Fatalf("Valid() was false immediately after Next() returned true")
			}

			key := it.Key()
			_ = it.RawKey()
			_ = it.Value()

			if prevKey != nil {
				if binary.CompareInternalKey(*prevKey, key) >= 0 {
					t.Fatalf("fuzzed iterator emitted out-of-order records: prev=%s, curr=%s", prevKey, key)
				}
			}
			k := key
			prevKey = &k
		}

		// Probe Seek and SeekToFirst on fuzzed iterator
		_ = it.Seek([]byte("probe"))
		_ = it.SeekToFirst()
		_ = it.Close()
	})
}
