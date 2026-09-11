package sstable

import "os"

// SetWriteFnForTesting exports writeFn seam injection on TableWriter for deterministic fault injection.
func (w *TableWriter) SetWriteFnForTesting(fn func(f *os.File, p []byte) (int, error)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writeFn = fn
}

// SetSyncFnForTesting exports syncFn seam injection on TableWriter for deterministic fault injection.
func (w *TableWriter) SetSyncFnForTesting(fn func(f *os.File) error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.syncFn = fn
}

// SetCloseFnForTesting exports closeFn seam injection on TableWriter for deterministic fault injection.
func (w *TableWriter) SetCloseFnForTesting(fn func(f *os.File) error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closeFn = fn
}
