package cache_test

import (
	"bytes"
	"testing"

	"github.com/silent-knight19/lattice/internal/cache"
)

// FuzzLRUShard fuzzes the LRUShard implementation against arbitrary operation sequences,
// capacities, keys, and values, asserting that invariants never break and no panic occurs.
func FuzzLRUShard(f *testing.F) {
	// Seed corpus with deterministic byte instructions
	f.Add([]byte{0x05, 0x01, 0x00, 0x01, 0x02, 0x03, 0x04})
	f.Add([]byte{0x10, 0x02, 0x05, 0x06, 0x07, 0x08, 0x09})
	f.Add([]byte{0x00, 0x01, 0x02, 0x03})
	f.Add([]byte{0x20, 0x01, 0x10, 0x20, 0x30, 0x40})

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < 2 {
			return
		}

		capacity := int(data[0] % 33) // capacity in [0..32]
		shard, err := cache.NewLRUShard(capacity)
		if err != nil {
			t.Fatalf("unexpected NewLRUShard error: %v", err)
		}

		stream := data[1:]
		for len(stream) >= 3 {
			op := stream[0] % 5
			fileNum := uint64(stream[1]%4 + 1)
			offset := uint64(stream[2]%16) * 4096
			key := cache.NewBlockKey(fileNum, offset)

			switch op {
			case 0, 1: // Get
				shard.Get(key)
			case 2: // Put
				valLen := int(stream[0] % 16)
				payload := bytes.Repeat([]byte{stream[1]}, valLen)
				shard.Put(key, payload)
			case 3: // Peek / Contains
				shard.Peek(key)
				shard.Contains(key)
			case 4: // Remove
				shard.Remove(key)
			}

			stream = stream[3:]

			if shard.Len() > capacity {
				t.Fatalf("shard length %d exceeded capacity %d", shard.Len(), capacity)
			}
			if err := shard.CheckInvariants(); err != nil {
				t.Fatalf("invariant broken during fuzzing: %v", err)
			}
		}
	})
}
