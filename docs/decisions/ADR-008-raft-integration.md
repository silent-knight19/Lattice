# ADR-008: Single-Group Raft Consensus for Distributed Replication (Version 1.1)

* **Status**: Accepted
* **Date**: 2026-09-06
* **Deciders**: Architecture & Distributed Systems Core Team
* **Technical Invariants Affected**: Clustering, Replication Invariants, Consistency Guarantees

---

## 1. Context
To provide fault tolerance and data safety across machine failures, Lattice must replicate data across multiple independent nodes over a network.

## 2. Problem
Simple primary-backup asynchronous replication suffers from data loss when the primary crashes before replicating logs to followers. Asynchronous replication also allows stale reads and split-brain conflicts during network partitions. Conversely, full Multi-Paxos or distributed transactions (2PC) introduce immense algorithmic complexity that would jeopardize project delivery.

## 3. Decision
We implement a **Single-Group Raft Consensus Module** in Version 1.1:
* Odd number of nodes ($N=3$ or $N=5$).
* Randomized election timers ($150-300\text{ms}$) with periodic heartbeats ($50\text{ms}$).
* Quorum write commits ($Q = \lfloor N/2 \rfloor + 1$).
* **Linearizable Reads via `ReadIndex`**: The leader confirms active majority leadership via heartbeats before serving reads from its local LSM state machine.

## 4. Alternatives Considered
1. **Primary-Backup Semi-Synchronous Replication**: Wait for 1 replica before ack; still vulnerable to complex edge cases during leader failover.
2. **Multi-Raft with Dynamic Sharding**: Scalable, but too complex for V1.1; deferred to Post-V1 roadmap.
3. **Multi-Paxos**: Equivalent safety, but symmetric leaderless consensus makes log compaction and dynamic leadership harder to reason about and implement.

## 5. Reasoning
* **Understandability & Invariants**: Raft decomposes consensus cleanly into Leader Election, Log Replication, and Safety Invariants.
* **Strict Linearizability**: The `ReadIndex` protocol mathematically eliminates stale reads under network partitions without the cost of writing reads to the replicated log.

## 6. Trade-offs
* **Single-Node Write Ceiling**: All writes must route through the single Raft leader; throughput does not scale horizontally by adding nodes.
* **Network Partition Latency**: If the leader is isolated in a minority partition, writes will stall or fail until a new leader is elected in the majority partition.

## 7. Consequences & Mitigations
* Follower nodes intercept client write requests and immediately return a redirect response containing the active leader's network address.

## 8. Evidence Classification
* **Theoretical Property**: Safety guaranteed across $F = \lfloor (N-1)/2 \rfloor$ concurrent node failures; zero split-brain writes under network partitions.
* **Observed Limitation**: Write throughput bounded by single-leader hardware capacity.
