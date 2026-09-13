package version

import "os"

// SetWriteFnForTesting replaces the low-level write function for ManifestWriter.
func (w *ManifestWriter) SetWriteFnForTesting(fn func(f *os.File, p []byte) (int, error)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writeFn = fn
}

// SetSyncFnForTesting replaces the low-level sync function for ManifestWriter.
func (w *ManifestWriter) SetSyncFnForTesting(fn func(f *os.File) error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.syncFn = fn
}

// SetCloseFnForTesting replaces the low-level close function for ManifestWriter.
func (w *ManifestWriter) SetCloseFnForTesting(fn func(f *os.File) error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closeFn = fn
}

// SetCurrentWriteFnForTesting temporarily replaces currentWriteFn and returns a restore closure.
func SetCurrentWriteFnForTesting(fn func(f *os.File, p []byte) (int, error)) func() {
	orig := currentWriteFn
	currentWriteFn = fn
	return func() { currentWriteFn = orig }
}

// SetCurrentSyncFnForTesting temporarily replaces currentSyncFn and returns a restore closure.
func SetCurrentSyncFnForTesting(fn func(f *os.File) error) func() {
	orig := currentSyncFn
	currentSyncFn = fn
	return func() { currentSyncFn = orig }
}

// SetCurrentCloseFnForTesting temporarily replaces currentCloseFn and returns a restore closure.
func SetCurrentCloseFnForTesting(fn func(f *os.File) error) func() {
	orig := currentCloseFn
	currentCloseFn = fn
	return func() { currentCloseFn = orig }
}

// SetCurrentRenameFnForTesting temporarily replaces currentRenameFn and returns a restore closure.
func SetCurrentRenameFnForTesting(fn func(oldpath, newpath string) error) func() {
	orig := currentRenameFn
	currentRenameFn = fn
	return func() { currentRenameFn = orig }
}

// SetCurrentSyncDirFnForTesting temporarily replaces currentSyncDirFn and returns a restore closure.
func SetCurrentSyncDirFnForTesting(fn func(dirPath string) error) func() {
	orig := currentSyncDirFn
	currentSyncDirFn = fn
	return func() { currentSyncDirFn = orig }
}

// SetCurrentOpenFnForTesting temporarily replaces currentOpenFn and returns a restore closure.
func SetCurrentOpenFnForTesting(fn func(name string) (*os.File, error)) func() {
	orig := currentOpenFn
	currentOpenFn = fn
	return func() { currentOpenFn = orig }
}

// SetCurrentReadFnForTesting temporarily replaces currentReadFn and returns a restore closure.
func SetCurrentReadFnForTesting(fn func(f *os.File, p []byte) (int, error)) func() {
	orig := currentReadFn
	currentReadFn = fn
	return func() { currentReadFn = orig }
}

// SetCurrentLstatFnForTesting temporarily replaces currentLstatFn and returns a restore closure.
func SetCurrentLstatFnForTesting(fn func(name string) (os.FileInfo, error)) func() {
	orig := currentLstatFn
	currentLstatFn = fn
	return func() { currentLstatFn = orig }
}

// SyncDirForTesting exposes the real parent-directory sync barrier so
// black-box regression tests can wrap currentSyncDirFn (to assert ordering
// and call counts) while still performing the actual directory fsync.
func SyncDirForTesting(dirPath string) error {
	return syncDir(dirPath)
}

// SetCleanupFnForTesting attaches an arbitrary callback invoked on the final 1->0 Unref transition.
func (v *Version) SetCleanupFnForTesting(fn func()) {
	v.cleanupFn = fn
}

// SetRefCountForTesting directly sets the atomic reference count of a Version for boundary testing.
// This is strictly a test-only helper located in export_test.go and is not part of the production API.
func (v *Version) SetRefCountForTesting(count int64) {
	v.refCount.Store(count)
}

// ActiveCurrentLockCount returns the current number of allocated entries in the directory lock registry.
func ActiveCurrentLockCount() int {
	return currentDirLocks.activeLockCount()
}

// SetBootLstatFnForTesting temporarily replaces bootLstatFn and returns a restore closure.
func SetBootLstatFnForTesting(fn func(name string) (os.FileInfo, error)) func() {
	orig := bootLstatFn
	bootLstatFn = fn
	return func() { bootLstatFn = orig }
}

// SetBootOpenFnForTesting temporarily replaces bootOpenFn and returns a restore closure.
func SetBootOpenFnForTesting(fn func(path string, flag int, perm os.FileMode) (*os.File, error)) func() {
	orig := bootOpenFn
	bootOpenFn = fn
	return func() { bootOpenFn = orig }
}

// SetBootPostOpenHookForTesting temporarily attaches a post-open hook and returns a restore closure.
func SetBootPostOpenHookForTesting(fn func(path string, f *os.File) error) func() {
	bootHookMu.Lock()
	orig := bootPostOpenHook
	bootPostOpenHook = fn
	bootHookMu.Unlock()
	return func() {
		bootHookMu.Lock()
		bootPostOpenHook = orig
		bootHookMu.Unlock()
	}
}

// SetReplayLstatFnForTesting temporarily replaces replayLstatFn and returns a restore closure.
func SetReplayLstatFnForTesting(fn func(name string) (os.FileInfo, error)) func() {
	orig := replayLstatFn
	replayLstatFn = fn
	return func() { replayLstatFn = orig }
}
