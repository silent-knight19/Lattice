package cache_test

import (
	"bytes"
	"testing"

	"github.com/silent-knight19/lattice/internal/cache"
)

// FuzzShardedCache fuzzes ShardedCache across arbitrary keys, capacities, offsets, and operation sequences.
func FuzzShardedCache(f *testing.F) {
	// Seed corpus with representative operation sequences
	f.Add([]byte{0x10, 0x01, 0x00, 0x10, 0x02, 0x05, 0x00, 0x20})
	f.Add([]byte{0x00, 0x02, 0x00, 0x08, 0x01, 0x01, 0x00, 0x10})
	f.Add([]byte{0x40, 0x03, 0x00, 0x40, 0x04, 0x08, 0x00, 0x80})

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < 2 {
			return
		}

		// Cap capacity between 0 and 128
		capacity := int(data[0] % 129)
		c, err := cache.NewShardedCache(capacity)
		if err != nil {
			t.Fatalf("unexpected NewShardedCache error: %v", err)
		}

		stream := data[1:]
		for len(stream) >= 4 {
			op := stream[0] % 5
			fileNum := uint64(stream[1]%16 + 1)
			offset := uint64(stream[2]%32) * 4096
			key := cache.NewBlockKey(fileNum, offset)

			// Assert shard index is strictly within bounds [0, 15]
			idx := cache.ShardIndex(key)
			if idx < 0 || idx >= cache.NumShards {
				t.Fatalf("ShardIndex(%v) = %d out of bounds [0, 15]", key, idx)
			}

			switch op {
			case 0, 1: // Get
				c.Get(key)
			case 2: // Put
				valLen := int(stream[3] % 32)
				payload := bytes.Repeat([]byte{stream[0]}, valLen)
				c.Put(key, payload)
			case 3: // Peek / Contains
				c.Peek(key)
				c.Contains(key)
			case 4: // Remove
				c.Remove(key)
			}

			stream = stream[4:]

			if c.Len() > capacity {
				t.Fatalf("cache total length %d exceeded capacity %d", c.Len(), capacity)
			}
		}

		// Assert invariants on each shard
		for i := 0; i < cache.NumShards; i++ {
			if err := c.Shard(i).CheckInvariants(); err != nil {
				t.Fatalf("shard %d invariant broken: %v", i, err)
			}
		}
	})
}
