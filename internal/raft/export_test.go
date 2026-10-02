package raft

import "os"

// SetRaftRenameFnForTesting overrides the rename function for fault-injection testing.
func SetRaftRenameFnForTesting(fn func(oldpath, newpath string) error) func() {
	orig := raftRenameFn
	raftRenameFn = fn
	return func() {
		raftRenameFn = orig
	}
}

// SetRaftSyncDirFnForTesting overrides the syncDir function for fault-injection testing.
func SetRaftSyncDirFnForTesting(fn func(dirPath string) error) func() {
	orig := raftSyncDirFn
	raftSyncDirFn = fn
	return func() {
		raftSyncDirFn = orig
	}
}

// SetRaftSyncLogFnForTesting overrides the fdatasync durability barrier on the
// in-place log append path for fault-injection testing.
func SetRaftSyncLogFnForTesting(fn func(f *os.File) error) func() {
	orig := raftSyncLogFn
	raftSyncLogFn = fn
	return func() {
		raftSyncLogFn = orig
	}
}

// SetRaftWriteLogFnForTesting overrides the in-place log append write for
// fault-injection testing.
func SetRaftWriteLogFnForTesting(fn func(f *os.File, b []byte) (int, error)) func() {
	orig := raftWriteLogFn
	raftWriteLogFn = fn
	return func() {
		raftWriteLogFn = orig
	}
}

// SetRaftSyncStateTmpFnForTesting overrides the fdatasync durability barrier on
// the HardState staging file for fault-injection testing.
func SetRaftSyncStateTmpFnForTesting(fn func(f *os.File) error) func() {
	orig := raftSyncStateTmpFn
	raftSyncStateTmpFn = fn
	return func() {
		raftSyncStateTmpFn = orig
	}
}

// SetRaftWriteStateTmpFnForTesting overrides the HardState staging write for
// fault-injection testing.
func SetRaftWriteStateTmpFnForTesting(fn func(f *os.File, b []byte) (int, error)) func() {
	orig := raftWriteStateTmpFn
	raftWriteStateTmpFn = fn
	return func() {
		raftWriteStateTmpFn = orig
	}
}

// IsPoisonedForTesting reports whether the Storage entered the terminal poisoned
// state due to a write or durability-barrier failure.
func (s *Storage) IsPoisonedForTesting() bool {
	if s == nil {
		return false
	}
	return s.poisoned.Load()
}

// SetRaftOpenFileFnForTesting overrides os.OpenFile for fault-injection testing.
func SetRaftOpenFileFnForTesting(fn func(name string, flag int, perm os.FileMode) (*os.File, error)) func() {
	orig := raftOpenFileFn
	raftOpenFileFn = fn
	return func() {
		raftOpenFileFn = orig
	}
}

// SetRaftCreateTmpFnForTesting overrides tmp file creation for fault-injection testing.
func SetRaftCreateTmpFnForTesting(fn func(path string, flag int, perm os.FileMode) (*os.File, error)) func() {
	orig := raftCreateTmpFn
	raftCreateTmpFn = fn
	return func() {
		raftCreateTmpFn = orig
	}
}

// SetRaftWriteTmpFnForTesting overrides tmp file writes for fault-injection testing.
func SetRaftWriteTmpFnForTesting(fn func(f *os.File, b []byte) (int, error)) func() {
	orig := raftWriteTmpFn
	raftWriteTmpFn = fn
	return func() {
		raftWriteTmpFn = orig
	}
}

// SetRaftSyncTmpFnForTesting overrides tmp file fsync for fault-injection testing.
func SetRaftSyncTmpFnForTesting(fn func(f *os.File) error) func() {
	orig := raftSyncTmpFn
	raftSyncTmpFn = fn
	return func() {
		raftSyncTmpFn = orig
	}
}

// SetRaftCloseTmpFnForTesting overrides tmp file close for fault-injection testing.
func SetRaftCloseTmpFnForTesting(fn func(f *os.File) error) func() {
	orig := raftCloseTmpFn
	raftCloseTmpFn = fn
	return func() {
		raftCloseTmpFn = orig
	}
}

// LogFileDescriptorNilForTesting reports whether the internal logFile descriptor is nil.
func (s *Storage) LogFileDescriptorNilForTesting() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.logFile == nil
}

// SetReadIndexTestHook sets an optional hook called during ReadIndex after sending
// heartbeats, before awaiting confirmation. This is strictly a test-only helper
// located in export_test.go and is not part of the production API.
func (n *Node) SetReadIndexTestHook(fn func()) {
	if n != nil {
		n.mu.Lock()
		n.readIndexTestHook = fn
		n.mu.Unlock()
	}
}

// SignalAppliedForTest allows test suites to deterministically simulate lastApplied advancement
// and wake barrier waiters. This is strictly a test-only helper located in export_test.go
// and is not part of the production API.
func (n *Node) SignalAppliedForTest(idx LogIndex) {
	if n == nil {
		return
	}
	n.mu.Lock()
	if idx > n.lastApplied {
		n.lastApplied = idx
	}
	n.mu.Unlock()
	n.notifyApplyWaiters(idx, nil)
}

// LeaderEpochForTest returns the volatile leaderEpoch counter for testing.
func (n *Node) LeaderEpochForTest() uint64 {
	if n == nil {
		return 0
	}
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.leaderEpoch
}
