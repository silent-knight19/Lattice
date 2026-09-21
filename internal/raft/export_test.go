package raft

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

// LogFileDescriptorNilForTesting reports whether the internal logFile descriptor is nil.
func (s *Storage) LogFileDescriptorNilForTesting() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.logFile == nil
}
