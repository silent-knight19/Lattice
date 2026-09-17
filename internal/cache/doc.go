// Package cache implements a high-throughput, 16-shard concurrent LRU Block Cache
// with 64-byte hardware cache line padding to prevent false sharing.
//
// Key Architectural Invariants:
//  1. 16 Independent Shards: Total cache capacity is partitioned deterministically across 16 shards.
//     Operations lock only the targeted shard mutex, with zero global locks on read/write paths.
//  2. Hardware Cache-Line Padding: Shard structures are padded to 128 bytes (2x 64-byte hardware cache lines)
//     to prevent CPU cache line bouncing and false sharing under high multi-core concurrency.
//  3. Deterministic Routing: Keys are routed via Murmur3-128 over a 16-byte buffer (FileNum || Offset).
//  4. Defensive Copy Isolation: Values are cloned via bytes.Clone on both Get and Put.
//  5. Capacity Model: Capacity is tracked in count of block entries (~4KB expected SSTable data block size).
package cache
