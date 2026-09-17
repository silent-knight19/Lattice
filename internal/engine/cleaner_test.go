package engine_test

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/engine"
	"github.com/silent-knight19/lattice/internal/sstable"
	"github.com/silent-knight19/lattice/internal/wal"
)

// hashFile computes the SHA-256 hash of a file on disk for byte-for-byte identity checks.
func hashFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Clean(path)) // #nosec G304 - test file verification
	if err != nil {
		t.Fatalf("hashFile failed for %s: %v", path, err)
	}
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// -----------------------------------------------------------------------------
// A. EMPTY DATABASE DIRECTORY (P07-S02-M02-INV-07)
// -----------------------------------------------------------------------------

func TestCleanOrphanedFiles_A_EmptyDirectory(t *testing.T) {
	dir := t.TempDir()

	report, err := engine.CleanOrphanedFilesDir(dir)
	if err != nil {
		t.Fatalf("CleanOrphanedFilesDir on empty dir failed: %v", err)
	}
	if report.CandidatesFound != 0 || report.FilesCleaned != 0 {
		t.Fatalf("expected 0 candidates and 0 cleaned, got %d and %d", report.CandidatesFound, report.FilesCleaned)
	}
	if len(report.Failures) != 0 {
		t.Fatalf("expected 0 failures, got: %v", report.Failures)
	}

	// Also verify via Engine method
	eng := engine.NewEngineWithOptions(engine.EngineOptions{
		DBPath:       dir,
		Backpressure: defaultEngineCfg(),
	})
	defer func() { _ = eng.Close() }()

	if err := eng.CleanOrphanedFiles(); err != nil {
		t.Fatalf("Engine.CleanOrphanedFiles on empty dir failed: %v", err)
	}
}

// -----------------------------------------------------------------------------
// B. NO TEMPORARY FILES PRESENT (P07-S02-M02-INV-02, INV-06)
// -----------------------------------------------------------------------------

func TestCleanOrphanedFiles_B_NoTemporaryFiles(t *testing.T) {
	dir := t.TempDir()

	// Populate standard persistent database files
	currentPath := filepath.Join(dir, "CURRENT")
	if err := os.WriteFile(currentPath, []byte("MANIFEST-000001\n"), 0600); err != nil { // #nosec G304
		t.Fatalf("failed to write CURRENT: %v", err)
	}

	manifestPath := filepath.Join(dir, "MANIFEST-000001")
	if err := os.WriteFile(manifestPath, []byte("manifest-payload-bytes"), 0600); err != nil { // #nosec G304
		t.Fatalf("failed to write MANIFEST: %v", err)
	}

	sstPath := filepath.Join(dir, "000001.sst")
	if err := os.WriteFile(sstPath, []byte("sstable-payload-bytes"), 0600); err != nil { // #nosec G304
		t.Fatalf("failed to write sstable: %v", err)
	}

	walDir := filepath.Join(dir, "wal")
	if err := os.Mkdir(walDir, 0700); err != nil {
		t.Fatalf("failed to mkdir wal: %v", err)
	}
	walPath := filepath.Join(walDir, "wal_000000000001.log")
	if err := os.WriteFile(walPath, []byte("wal-record-bytes"), 0600); err != nil { // #nosec G304
		t.Fatalf("failed to write wal segment: %v", err)
	}

	report, err := engine.CleanOrphanedFilesDir(dir)
	if err != nil {
		t.Fatalf("CleanOrphanedFilesDir failed: %v", err)
	}
	if report.CandidatesFound != 0 || report.FilesCleaned != 0 {
		t.Fatalf("expected 0 candidates found/cleaned, got %d and %d", report.CandidatesFound, report.FilesCleaned)
	}

	// Verify all persistent files remain intact
	for _, p := range []string{currentPath, manifestPath, sstPath, walPath} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("persistent file %s was missing: %v", p, err)
		}
	}
}

// -----------------------------------------------------------------------------
// C. ONE VALID ORPHAN TEMPORARY FILE (P07-S02-M02-INV-01, INV-05)
// -----------------------------------------------------------------------------

func TestCleanOrphanedFiles_C_OneValidOrphan(t *testing.T) {
	dir := t.TempDir()

	orphanName := ".tmp_000001.sst_9876543210"
	orphanPath := filepath.Join(dir, orphanName)
	if err := os.WriteFile(orphanPath, []byte("interrupted-staging-bytes"), 0600); err != nil { // #nosec G304
		t.Fatalf("failed to create orphan file: %v", err)
	}

	report, err := engine.CleanOrphanedFilesDir(dir)
	if err != nil {
		t.Fatalf("CleanOrphanedFilesDir failed: %v", err)
	}
	if report.CandidatesFound != 1 || report.FilesCleaned != 1 {
		t.Fatalf("expected 1 candidate and 1 cleaned, got %d and %d", report.CandidatesFound, report.FilesCleaned)
	}
	if len(report.Failures) != 0 {
		t.Fatalf("unexpected failures: %v", report.Failures)
	}

	// Verify orphan file was unlinked
	if _, err := os.Lstat(orphanPath); !os.IsNotExist(err) {
		t.Fatalf("orphan file %s still exists on disk after cleanup", orphanPath)
	}
}

// -----------------------------------------------------------------------------
// D. MULTIPLE ORPHAN TEMPORARY FILES (P07-S02-M02-INV-01, INV-05)
// -----------------------------------------------------------------------------

func TestCleanOrphanedFiles_D_MultipleOrphans(t *testing.T) {
	dir := t.TempDir()

	orphans := []string{
		".tmp_000001.sst_11111",
		".tmp_000002.sst_22222",
		".tmp_000003.sst_33333",
		".tmp_000042.sst_abcde12345",
	}

	for _, name := range orphans {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("staging-"+name), 0600); err != nil { // #nosec G304
			t.Fatalf("failed to create orphan %s: %v", name, err)
		}
	}

	report, err := engine.CleanOrphanedFilesDir(dir)
	if err != nil {
		t.Fatalf("CleanOrphanedFilesDir failed: %v", err)
	}
	if report.CandidatesFound != len(orphans) || report.FilesCleaned != len(orphans) {
		t.Fatalf("expected %d candidates/cleaned, got %d found, %d cleaned", len(orphans), report.CandidatesFound, report.FilesCleaned)
	}

	for _, name := range orphans {
		p := filepath.Join(dir, name)
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Fatalf("orphan %s still exists after cleanup", name)
		}
	}
}

// -----------------------------------------------------------------------------
// E. MIXED PERSISTENT FILES & BYTE-FOR-BYTE PRESERVATION (INV-02, INV-06, INV-10)
// -----------------------------------------------------------------------------

func TestCleanOrphanedFiles_E_MixedPersistentFiles(t *testing.T) {
	dir := t.TempDir()

	// 1. Create live persistent database files
	currentPath := filepath.Join(dir, "CURRENT")
	manifestPath := filepath.Join(dir, "MANIFEST-000001")
	sstPath1 := filepath.Join(dir, "000001.sst")
	sstPath2 := filepath.Join(dir, "000002.sst")
	walDir := filepath.Join(dir, "wal")
	if err := os.Mkdir(walDir, 0700); err != nil {
		t.Fatalf("mkdir wal failed: %v", err)
	}
	walPath := filepath.Join(walDir, "wal_000000000001.log")

	if err := os.WriteFile(currentPath, []byte("MANIFEST-000001\n"), 0600); err != nil { // #nosec G304
		t.Fatalf("write CURRENT failed: %v", err)
	}
	if err := os.WriteFile(manifestPath, []byte("manifest-durable-data-12345"), 0600); err != nil { // #nosec G304
		t.Fatalf("write MANIFEST failed: %v", err)
	}
	if err := os.WriteFile(sstPath1, []byte("sstable-data-000001"), 0600); err != nil { // #nosec G304
		t.Fatalf("write sst1 failed: %v", err)
	}
	if err := os.WriteFile(sstPath2, []byte("sstable-data-000002"), 0600); err != nil { // #nosec G304
		t.Fatalf("write sst2 failed: %v", err)
	}
	if err := os.WriteFile(walPath, []byte("wal-durable-log-data"), 0600); err != nil { // #nosec G304
		t.Fatalf("write wal failed: %v", err)
	}

	// Compute pre-cleanup cryptographic hashes
	hashCurrentBefore := hashFile(t, currentPath)
	hashManifestBefore := hashFile(t, manifestPath)
	hashSST1Before := hashFile(t, sstPath1)
	hashSST2Before := hashFile(t, sstPath2)
	hashWALBefore := hashFile(t, walPath)

	// 2. Add orphan staging files
	orphan1 := filepath.Join(dir, ".tmp_000003.sst_orphan123")
	orphan2 := filepath.Join(dir, ".tmp_000004.sst_orphan456")
	if err := os.WriteFile(orphan1, []byte("orphan-1"), 0600); err != nil { // #nosec G304
		t.Fatalf("write orphan1 failed: %v", err)
	}
	if err := os.WriteFile(orphan2, []byte("orphan-2"), 0600); err != nil { // #nosec G304
		t.Fatalf("write orphan2 failed: %v", err)
	}

	// 3. Execute cleanup
	report, err := engine.CleanOrphanedFilesDir(dir)
	if err != nil {
		t.Fatalf("CleanOrphanedFilesDir failed: %v", err)
	}
	if report.CandidatesFound != 2 || report.FilesCleaned != 2 {
		t.Fatalf("expected 2 candidates and 2 cleaned, got %d and %d", report.CandidatesFound, report.FilesCleaned)
	}

	// 4. Verify orphans are gone
	for _, op := range []string{orphan1, orphan2} {
		if _, err := os.Lstat(op); !os.IsNotExist(err) {
			t.Fatalf("orphan %s was not cleaned up", op)
		}
	}

	// 5. Verify persistent files exist and hashes are strictly identical before and after
	if hashFile(t, currentPath) != hashCurrentBefore {
		t.Fatalf("CURRENT was mutated during orphan cleanup!")
	}
	if hashFile(t, manifestPath) != hashManifestBefore {
		t.Fatalf("MANIFEST was mutated during orphan cleanup!")
	}
	if hashFile(t, sstPath1) != hashSST1Before {
		t.Fatalf("000001.sst was mutated during orphan cleanup!")
	}
	if hashFile(t, sstPath2) != hashSST2Before {
		t.Fatalf("000002.sst was mutated during orphan cleanup!")
	}
	if hashFile(t, walPath) != hashWALBefore {
		t.Fatalf("wal was mutated during orphan cleanup!")
	}
}

// -----------------------------------------------------------------------------
// F. SYMLINK MASQUERADE & VICTIM PROTECTION (INV-03) (SECTION 32 & 50)
// -----------------------------------------------------------------------------

func TestCleanOrphanedFiles_F_SymlinkVictimProtection(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation may require elevated privileges on Windows")
	}

	dir := t.TempDir()
	outsideDir := t.TempDir()

	// Create an external victim file that an attacker targets
	victimPath := filepath.Join(outsideDir, "critical_system_file.txt")
	const victimContent = "CONFIDENTIAL_PAYLOAD_DO_NOT_DELETE"
	if err := os.WriteFile(victimPath, []byte(victimContent), 0600); err != nil { // #nosec G304
		t.Fatalf("failed to write victim file: %v", err)
	}
	victimHashBefore := hashFile(t, victimPath)

	// Create a symlink masquerading as a temporary staging file inside db directory
	symlinkPath := filepath.Join(dir, ".tmp_000042.sst_attack")
	if err := os.Symlink(victimPath, symlinkPath); err != nil {
		t.Fatalf("failed to create symlink: %v", err)
	}

	// Also create a legitimate orphan file
	legitOrphan := filepath.Join(dir, ".tmp_000042.sst_legit123")
	if err := os.WriteFile(legitOrphan, []byte("legit-staging"), 0600); err != nil { // #nosec G304
		t.Fatalf("failed to write legit orphan: %v", err)
	}

	// Run cleanup
	report, err := engine.CleanOrphanedFilesDir(dir)
	// Must return an error or capture the security failure
	if err == nil {
		t.Fatalf("expected error or failure reporting for symlink candidate")
	}
	if report.CandidatesFound != 2 {
		t.Fatalf("expected 2 candidates found, got %d", report.CandidatesFound)
	}
	if report.FilesCleaned != 1 {
		t.Fatalf("expected 1 file cleaned (the legit one), got %d", report.FilesCleaned)
	}
	if _, hasSymlinkFailure := report.Failures[".tmp_000042.sst_attack"]; !hasSymlinkFailure {
		t.Fatalf("expected symlink candidate in failures map, got: %v", report.Failures)
	}

	// Invariant Check: Victim file outside the database MUST remain 100% untouched and byte-identical!
	if _, err := os.Stat(victimPath); err != nil {
		t.Fatalf("SECURITY BREACH: victim file %s was deleted!", victimPath)
	}
	if hashFile(t, victimPath) != victimHashBefore {
		t.Fatalf("SECURITY BREACH: victim file %s content was modified!", victimPath)
	}

	// Legitimate orphan must be removed
	if _, err := os.Lstat(legitOrphan); !os.IsNotExist(err) {
		t.Fatalf("legit orphan %s still exists", legitOrphan)
	}
}

// -----------------------------------------------------------------------------
// G. TEMPORARY DIRECTORY MASQUERADE IS NEVER RECURSIVELY REMOVED (INV-04, INV-12)
// -----------------------------------------------------------------------------

func TestCleanOrphanedFiles_G_DirectoryMasquerade(t *testing.T) {
	dir := t.TempDir()

	// Attacker creates a subdirectory with a name that matches the temporary grammar
	fakeTempDir := filepath.Join(dir, ".tmp_000042.sst_directory")
	if err := os.Mkdir(fakeTempDir, 0700); err != nil {
		t.Fatalf("failed to mkdir: %v", err)
	}
	nestedFile := filepath.Join(fakeTempDir, "important_nested_data.txt")
	if err := os.WriteFile(nestedFile, []byte("nested-data"), 0600); err != nil { // #nosec G304
		t.Fatalf("failed to write nested file: %v", err)
	}

	report, err := engine.CleanOrphanedFilesDir(dir)
	if err == nil {
		t.Fatalf("expected error on directory masquerade")
	}
	if report.FilesCleaned != 0 {
		t.Fatalf("expected 0 files cleaned, got %d", report.FilesCleaned)
	}
	if _, hasDirFailure := report.Failures[".tmp_000042.sst_directory"]; !hasDirFailure {
		t.Fatalf("expected directory masquerade in failures: %v", report.Failures)
	}

	// Verify directory and nested file were NOT deleted
	if _, err := os.Stat(fakeTempDir); err != nil {
		t.Fatalf("directory was unexpectedly removed!")
	}
	if _, err := os.Stat(nestedFile); err != nil {
		t.Fatalf("nested file was unexpectedly removed!")
	}
}

// -----------------------------------------------------------------------------
// H. SPECIAL OBJECT (FIFO) MASQUERADE (INV-04)
// -----------------------------------------------------------------------------

func TestCleanOrphanedFiles_H_FIFOMasquerade(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("FIFO creation not supported on Windows")
	}

	dir := t.TempDir()
	fifoPath := filepath.Join(dir, ".tmp_000042.sst_namedpipe")
	if err := syscall.Mkfifo(fifoPath, 0600); err != nil {
		t.Skipf("Mkfifo not permitted or supported in this environment: %v", err)
	}

	report, err := engine.CleanOrphanedFilesDir(dir)
	if err == nil {
		t.Fatalf("expected error when encountering non-regular file candidate")
	}
	if report.FilesCleaned != 0 {
		t.Fatalf("expected 0 files cleaned, got %d", report.FilesCleaned)
	}
	if _, hasFIFOFailure := report.Failures[".tmp_000042.sst_namedpipe"]; !hasFIFOFailure {
		t.Fatalf("expected FIFO in failures: %v", report.Failures)
	}

	// Clean up FIFO manually
	_ = os.Remove(fifoPath)
}

// -----------------------------------------------------------------------------
// I. UNKNOWN .TMP-LOOKING FILES PRESERVED (INV-11) (SECTION 33)
// -----------------------------------------------------------------------------

func TestCleanOrphanedFiles_I_UnknownTmpFilesPreserved(t *testing.T) {
	dir := t.TempDir()

	// Files that have .tmp in their name or look temporary, but are NOT SSTable staging files
	nearMissFiles := []string{
		"important.tmp.backup",
		"unknown.tmp",
		"CURRENT.tmp",
		"000001.sst.tmp",
		".tmp_backup",
		".tmp_000001.sst",
		".tmp_table.data",
		"table-staging.data",
		"MANIFEST.tmp",
	}

	for _, name := range nearMissFiles {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("payload-"+name), 0600); err != nil { // #nosec G304
			t.Fatalf("failed to write %s: %v", name, err)
		}
	}

	report, err := engine.CleanOrphanedFilesDir(dir)
	if err != nil {
		t.Fatalf("CleanOrphanedFilesDir failed: %v", err)
	}
	if report.CandidatesFound != 0 || report.FilesCleaned != 0 {
		t.Fatalf("expected 0 candidates and 0 cleaned for near-miss files, got %d and %d", report.CandidatesFound, report.FilesCleaned)
	}

	// Verify all near-miss files remain on disk intact
	for _, name := range nearMissFiles {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("near-miss file %s was unexpectedly deleted!", name)
		}
	}
}

// -----------------------------------------------------------------------------
// J. PATH TRAVERSAL ATTEMPT REJECTION (INV-05)
// -----------------------------------------------------------------------------

func TestCleanOrphanedFiles_J_PathTraversalUnitTests(t *testing.T) {
	traversalNames := []string{
		".tmp_../etc/passwd.sst_123",
		".tmp_..\\windows\\system32.sst_123",
		".tmp_/rooted/path.sst_123",
		".tmp_\x00evil.sst_123",
		".tmp_..sst_123",
		".tmp_.sst_123",
		".tmp_valid.sst_",
		".tmp_valid.sst_invalid/suffix",
		"../.tmp_000001.sst_123",
	}

	for _, name := range traversalNames {
		if engine.IsOrphanStagingFile(name) {
			t.Fatalf("IsOrphanStagingFile unexpectedly accepted dangerous name: %q", name)
		}
	}
}

// -----------------------------------------------------------------------------
// K. DELETION PERMISSION FAILURE EXPLICIT SEMANTICS (INV-08)
// -----------------------------------------------------------------------------

func TestCleanOrphanedFiles_K_DeletionPermissionFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod-based directory permission restrictions behave differently on Windows")
	}

	dir := t.TempDir()
	orphanName := ".tmp_000001.sst_unremovable"
	orphanPath := filepath.Join(dir, orphanName)
	if err := os.WriteFile(orphanPath, []byte("data"), 0600); err != nil { // #nosec G304
		t.Fatalf("write failed: %v", err)
	}

	// Make directory read-only so unlink fails with EACCES / EPERM
	if os.Geteuid() == 0 {
		t.Skip("skipping permission-dependent test when running as root (UID 0)")
	}
	if err := os.Chmod(dir, 0500); err != nil { // #nosec G302 - test-only permission restriction
		t.Fatalf("chmod dir failed: %v", err)
	}
	defer func() { _ = os.Chmod(dir, 0700) }() // #nosec G302 - test cleanup

	report, err := engine.CleanOrphanedFilesDir(dir)
	if err == nil {
		t.Fatalf("expected error when deletion fails due to permissions")
	}
	if report.CandidatesFound != 1 {
		t.Fatalf("expected 1 candidate found, got %d", report.CandidatesFound)
	}
	if report.FilesCleaned != 0 {
		t.Fatalf("expected 0 files cleaned on permission failure, got %d", report.FilesCleaned)
	}
	if _, ok := report.Failures[orphanName]; !ok {
		t.Fatalf("expected failure for %s in report: %v", orphanName, report.Failures)
	}
}

// -----------------------------------------------------------------------------
// L. REPEATED CLEANUP IDEMPOTENCE (INV-07)
// -----------------------------------------------------------------------------

func TestCleanOrphanedFiles_L_Idempotence(t *testing.T) {
	dir := t.TempDir()

	orphan1 := filepath.Join(dir, ".tmp_000001.sst_run1")
	orphan2 := filepath.Join(dir, ".tmp_000002.sst_run1")
	_ = os.WriteFile(orphan1, []byte("1"), 0600) // #nosec G304
	_ = os.WriteFile(orphan2, []byte("2"), 0600) // #nosec G304

	// First pass: deletes both files
	report1, err1 := engine.CleanOrphanedFilesDir(dir)
	if err1 != nil {
		t.Fatalf("pass 1 failed: %v", err1)
	}
	if report1.CandidatesFound != 2 || report1.FilesCleaned != 2 {
		t.Fatalf("pass 1: expected 2 cleaned, got %d", report1.FilesCleaned)
	}

	// Second pass: directory unchanged, candidate set empty, must succeed cleanly
	report2, err2 := engine.CleanOrphanedFilesDir(dir)
	if err2 != nil {
		t.Fatalf("pass 2 failed: %v", err2)
	}
	if report2.CandidatesFound != 0 || report2.FilesCleaned != 0 {
		t.Fatalf("pass 2: expected 0 candidates/cleaned, got %d and %d", report2.CandidatesFound, report2.FilesCleaned)
	}
	if len(report2.Failures) != 0 {
		t.Fatalf("pass 2: unexpected failures: %v", report2.Failures)
	}
}

// -----------------------------------------------------------------------------
// M. REAL TABLEWRITER-PRODUCED STAGING ARTIFACT (SECTION 34)
// -----------------------------------------------------------------------------

func TestCleanOrphanedFiles_M_RealTableWriterStagingArtifact(t *testing.T) {
	dir := t.TempDir()
	sstDst := filepath.Join(dir, "000001.sst")

	// Create real TableWriter
	w, err := sstable.NewTableWriter(sstDst, sstable.DefaultTableWriterOptions())
	if err != nil {
		t.Fatalf("NewTableWriter failed: %v", err)
	}

	// Add records
	ik, err := binary.NewInternalKey([]byte("key1"), 10, binary.OpTypePut)
	if err != nil {
		t.Fatalf("NewInternalKey failed: %v", err)
	}
	if err := w.Add(ik, []byte("value1")); err != nil {
		t.Fatalf("Add failed: %v", err)
	}

	// Retrieve real staging file path created by TableWriter
	realTmpPath := w.TempPath()
	if realTmpPath == "" {
		t.Fatalf("TableWriter did not provide TempPath")
	}
	if _, err := os.Stat(realTmpPath); err != nil {
		t.Fatalf("real staging file %s does not exist on disk: %v", realTmpPath, err)
	}

	// Verify that realTmpPath matches IsOrphanStagingFile grammar
	baseTmpName := filepath.Base(realTmpPath)
	if !engine.IsOrphanStagingFile(baseTmpName) {
		t.Fatalf("real TableWriter staging name %q failed IsOrphanStagingFile check!", baseTmpName)
	}

	// Execute CleanOrphanedFiles
	report, err := engine.CleanOrphanedFilesDir(dir)
	if err != nil {
		t.Fatalf("CleanOrphanedFilesDir failed: %v", err)
	}
	if report.CandidatesFound != 1 || report.FilesCleaned != 1 {
		t.Fatalf("expected real staging file to be detected and cleaned, got %d found, %d cleaned", report.CandidatesFound, report.FilesCleaned)
	}

	// Assert staging file is deleted
	if _, err := os.Lstat(realTmpPath); !os.IsNotExist(err) {
		t.Fatalf("real staging file %s still exists after cleanup!", realTmpPath)
	}

	// Assert final SSTable was NOT fabricated
	if _, err := os.Lstat(sstDst); !os.IsNotExist(err) {
		t.Fatalf("final SSTable %s was unexpectedly created!", sstDst)
	}
}

// -----------------------------------------------------------------------------
// N. REALISTIC CRASH RECOVERY PIPELINE INTEGRATION (SECTION 0, 46, 51)
// -----------------------------------------------------------------------------

func TestCleanOrphanedFiles_N_CrashRecoveryPipelineIntegration(t *testing.T) {
	dir := t.TempDir()

	// 1. Establish durable MANIFEST with LastSeqNum = 50
	replayRes := setupTestManifestWithCheckpoint(t, dir, 50)
	replayRes.Version.Unref()

	// 2. Add WAL records with seq 51..60 (uncommitted writes)
	records := make([]wal.Record, 10)
	for i := 0; i < 10; i++ {
		seq := uint64(51 + i)
		records[i] = makePutRecord(seq, fmt.Sprintf("k%d", seq), fmt.Sprintf("v%d", seq))
	}
	writeWALSegment(t, dir, 1, records...)

	// 3. Inject realistic orphan staging file from an interrupted flush
	orphanPath := filepath.Join(dir, ".tmp_000002.sst_interruptedflush")
	if err := os.WriteFile(orphanPath, []byte("partial-sstable-data"), 0600); err != nil { // #nosec G304
		t.Fatalf("failed to write orphan staging file: %v", err)
	}

	// 4. Initialize Engine and run RecoverWAL (orchestrates MANIFEST + WAL + Orphan Cleanup)
	eng := engine.NewEngineWithOptions(engine.EngineOptions{
		DBPath:       dir,
		Backpressure: defaultEngineCfg(),
	})
	defer func() { _ = eng.Close() }()

	if err := eng.RecoverWAL(); err != nil {
		t.Fatalf("RecoverWAL failed: %v", err)
	}

	// 5. Assert MemTable contains uncommitted records (51..60)
	if eng.ActiveMemTable().Len() != 10 {
		t.Fatalf("expected 10 records in MemTable, got %d", eng.ActiveMemTable().Len())
	}
	val, err := eng.Get([]byte("k51"))
	if err != nil || string(val) != "v51" {
		t.Fatalf("Get(k51) failed: %v", err)
	}

	// 6. Assert orphan staging file was automatically cleaned up
	if _, err := os.Lstat(orphanPath); !os.IsNotExist(err) {
		t.Fatalf("orphan staging file was not purged by RecoverWAL pipeline!")
	}
}

// -----------------------------------------------------------------------------
// O. DIRECT ENTRY ALLOWLIST DECISION TABLE (SECTION 45)
// -----------------------------------------------------------------------------

func TestCleanOrphanedFiles_O_DecisionTable(t *testing.T) {
	testCases := []struct {
		filename    string
		shouldClean bool
		desc        string
	}{
		{"CURRENT", false, "Authoritative pointer"},
		{"CURRENT.tmp", false, "CURRENT writer staging file"},
		{"MANIFEST-000001", false, "Active manifest log"},
		{"000001.sst", false, "Live SSTable"},
		{"000042.sst", false, "Live SSTable"},
		{".tmp_000001.sst_123456", true, "Exact orphan staging file"},
		{".tmp_000042.sst_999999", true, "Exact orphan staging file"},
		{"unknown.tmp", false, "Unknown tmp file"},
		{"important.tmp.backup", false, "Near-miss file"},
		{".tmp_backup", false, "Non-SSTable tmp prefix"},
		{"000001.sst.tmp", false, "Legacy tmp suffix"},
	}

	for _, tc := range testCases {
		res := engine.IsOrphanStagingFile(tc.filename)
		if res != tc.shouldClean {
			t.Errorf("IsOrphanStagingFile(%q) [%s] = %v; want %v", tc.filename, tc.desc, res, tc.shouldClean)
		}
	}
}
