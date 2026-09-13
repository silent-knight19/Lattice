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
