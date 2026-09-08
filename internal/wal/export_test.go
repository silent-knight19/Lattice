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

// RecoverSegmentWithSeamsForTesting exports recoverSegmentWithSeams for white-box fault injection tests.
func RecoverSegmentWithSeamsForTesting(
	path string,
	syncFn func(f *os.File) error,
	truncateFn func(f *os.File, size int64) error,
) (RecoveryResult, error) {
	return recoverSegmentWithSeams(path, syncFn, truncateFn)
}
