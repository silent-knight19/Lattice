package wal

import "os"

// SetSyncFnForTesting exports setSyncFnForTesting for white-box testing from external test packages.
func (w *WALWriter) SetSyncFnForTesting(fn func(f *os.File) error) {
	w.setSyncFnForTesting(fn)
}

// SetWriteFnForTesting exports setWriteFnForTesting for white-box testing from external test packages.
func (w *WALWriter) SetWriteFnForTesting(fn func(f *os.File, p []byte) (int, error)) {
	w.setWriteFnForTesting(fn)
}
