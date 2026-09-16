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

// SetSyncDirFnForTesting exports syncDirFn seam injection on TableWriter for deterministic fault injection.
func (w *TableWriter) SetSyncDirFnForTesting(fn func(dirPath string) error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.syncDirFn = fn
}

// SetLinkFnForTesting exports linkFn seam injection on TableWriter for deterministic fault injection.
func (w *TableWriter) SetLinkFnForTesting(fn func(oldname, newname string) error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.linkFn = fn
}

// SetSyncDirFileFnForTesting exports syncDirFileFn seam injection on TableWriter for deterministic fault injection.
func (w *TableWriter) SetSyncDirFileFnForTesting(fn func(f *os.File) error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.syncDirFileFn = fn
}

// SetPreLinkHookForTesting injects a test hook executed immediately prior to publication linkFn.
func (w *TableWriter) SetPreLinkHookForTesting(fn func() error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.preLinkHookFn = fn
}

// PinnedParentDirStat returns the FileInfo of the pinned parent directory descriptor.
func (w *TableWriter) PinnedParentDirStat() os.FileInfo {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.parentDirStat
}

// PinnedParentDirFile returns the open *os.File descriptor of the pinned parent directory.
func (w *TableWriter) PinnedParentDirFile() *os.File {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.parentDirFile
}

// SetReadAtFnForTesting replaces the low-level positional read function for TableReader.
func (r *TableReader) SetReadAtFnForTesting(fn func(p []byte, off int64) (int, error)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.readAtFn = fn
}

// ReadAtFnForTesting returns the current low-level positional read function for TableReader.
func (r *TableReader) ReadAtFnForTesting() func(p []byte, off int64) (int, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.readAtFn
}

// SearchDataBlockForTesting exports searchDataBlock for isolated unit testing and fuzzing.
func SearchDataBlockForTesting(blockBuf []byte, targetUserKey []byte, blockOffset uint64) ([]byte, error) {
	return searchDataBlock(blockBuf, targetUserKey, blockOffset)
}

// SetPostOpenHookForTesting injects a test hook executed immediately after file open and before post-open validation.
// Returns a cleanup function that restores the prior hook.
func SetPostOpenHookForTesting(fn func() error) func() {
	postOpenHookMu.Lock()
	old := postOpenHookFn
	postOpenHookFn = fn
	postOpenHookMu.Unlock()
	return func() {
		postOpenHookMu.Lock()
		postOpenHookFn = old
		postOpenHookMu.Unlock()
	}
}

// SetOpenFileFnForTesting replaces the low-level file open function for TableReader.
// Returns a cleanup function that restores the prior open function.
func SetOpenFileFnForTesting(fn func(name string) (*os.File, error)) func() {
	postOpenHookMu.Lock()
	old := openFileFn
	openFileFn = fn
	postOpenHookMu.Unlock()
	return func() {
		postOpenHookMu.Lock()
		openFileFn = old
		postOpenHookMu.Unlock()
	}
}

// SetFooterForTesting replaces the footer in TableReader for testing edge-case corrupted footers.
func SetFooterForTesting(r *TableReader, f Footer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.footer = f
}
