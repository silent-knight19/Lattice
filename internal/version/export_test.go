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
