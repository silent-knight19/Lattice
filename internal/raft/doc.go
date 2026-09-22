// Package raft implements the single-group Raft consensus engine: persistent
// consensus state, leader election with randomized timers, quorum vote
// counting, heartbeat scheduling, proposal ingestion, follower log
// replication, and leader quorum commitment (volatile commitIndex).
//
// Explicitly out of scope here: state-machine application and client commit
// acknowledgement (Phase 16), the ReadIndex protocol for linearizable reads
// (Phase 17), snapshots, membership changes, and multi-group Raft.
package raft
