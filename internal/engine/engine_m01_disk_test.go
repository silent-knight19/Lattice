package engine_test

import (
	"bytes"
	stdErrors "errors"
	"os"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/cache"
	"github.com/silent-knight19/lattice/internal/engine"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/sstable"
	"github.com/silent-knight19/lattice/internal/version"
)

// buildSSTable writes sorted PUT keys to an SSTable file and installs it into
// eng's VersionSet at level. Keys must already be sorted ascending.
func buildSSTable(t *testing.T, eng *engine.Engine, dbPath string, fileNum uint64, level int, keys []string, vals []string, seqBase uint64) version.FileMetadata {
	t.Helper()
	if len(keys) != len(vals) {
		t.Fatalf("keys/vals length mismatch")
	}
	path := version.TablePath(dbPath, fileNum)
	w, err := sstable.NewTableWriter(path, sstable.DefaultTableWriterOptions())
	if err != nil {
		t.Fatalf("NewTableWriter failed: %v", err)
	}
	for i := range keys {
		ik, err := binary.NewInternalKey([]byte(keys[i]), binary.SeqNum(seqBase+uint64(i)), binary.OpTypePut)
		if err != nil {
			t.Fatalf("NewInternalKey failed: %v", err)
		}
		if err := w.Add(ik, []byte(vals[i])); err != nil {
			_ = w.Close()
			t.Fatalf("TableWriter.Add failed: %v", err)
		}
	}
	meta, err := w.Finish()
	if err != nil {
		t.Fatalf("TableWriter.Finish failed: %v", err)
	}
	fm := version.NewFileMetadataFromSSTable(fileNum, meta)
	vs := eng.VersionSet()
	cur := vs.Current()
	var levels [version.NumLevels][]version.FileMetadata
	if cur != nil {
		for lvl := 0; lvl < version.NumLevels; lvl++ {
			levels[lvl] = append(levels[lvl], cur.Files(lvl)...)
		}
		cur.Unref()
	}
	levels[level] = append(levels[level], fm)
	v := version.NewVersion(levels)
	if err := vs.AppendVersion(v); err != nil {
		v.Unref()
		t.Fatalf("AppendVersion failed: %v", err)
	}
	return fm
}

func newDiskEngine(t *testing.T, cacheCap int) (*engine.Engine, string) {
	t.Helper()
	dir := t.TempDir()
	var bc *cache.ShardedCache
	if cacheCap > 0 {
		var err error
		bc, err = cache.NewShardedCache(cacheCap)
		if err != nil {
			t.Fatalf("NewShardedCache failed: %v", err)
		}
	}
	eng := engine.NewEngineWithOptions(engine.EngineOptions{
		DBPath: dir,
		Backpressure: engine.BackpressureConfig{
			MaxMemoryBytes: 64 * 1024 * 1024,
			HighWatermark:  0.80,
			HardWatermark:  0.95,
			MaxWaitTimeout: time.Second,
		},
		BlockCache: bc,
	})
	if err := eng.Open(); err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	return eng, dir
}

func TestEngineM01_PersistedReads(t *testing.T) {
	eng, dir := newDiskEngine(t, 256)
	defer func() { _ = eng.Close() }()

	// Persist 3 keys to L0 via real SSTable + VersionSet (no Engine flush).
	buildSSTable(t, eng, dir, 1, 0, []string{"disk_a", "disk_b", "disk_c"}, []string{"va", "vb", "vc"}, 1)

	for _, tc := range []struct{ k, v string }{{"disk_a", "va"}, {"disk_b", "vb"}, {"disk_c", "vc"}} {
		got, err := eng.Get([]byte(tc.k))
		if err != nil {
			t.Fatalf("Get persisted %q failed: %v", tc.k, err)
		}
		if !bytes.Equal(got, []byte(tc.v)) {
			t.Fatalf("persisted %q: got %q want %q", tc.k, got, tc.v)
		}
	}

	// Missing persisted key => NotFound (not error).
	if _, err := eng.Get([]byte("disk_missing")); !stdErrors.Is(err, errors.ErrKeyNotFound) {
		t.Fatalf("expected NotFound for missing disk key, got %v", err)
	}
}

func TestEngineM01_TombstoneShadowsSSTable(t *testing.T) {
	eng, dir := newDiskEngine(t, 256)
	defer func() { _ = eng.Close() }()
	ctx := testCtx()

	buildSSTable(t, eng, dir, 1, 0, []string{"shadow"}, []string{"V1"}, 1)

	// Newer MemTable value shadows older SSTable value.
	if err := eng.Put(ctx, []byte("shadow"), []byte("V2")); err != nil {
		t.Fatalf("Put failed: %v", err)
	}
	if got, _ := eng.Get([]byte("shadow")); !bytes.Equal(got, []byte("V2")) {
		t.Fatalf("expected V2, got %q", got)
	}

	// Newer tombstone hides older SSTable value.
	if err := eng.Delete(ctx, []byte("shadow")); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	if _, err := eng.Get([]byte("shadow")); !stdErrors.Is(err, errors.ErrKeyNotFound) {
		t.Fatalf("expected NotFound after tombstone, got %v", err)
	}
}

func TestEngineM01_UpdatePersistedKey(t *testing.T) {
	eng, dir := newDiskEngine(t, 256)
	defer func() { _ = eng.Close() }()
	ctx := testCtx()

	buildSSTable(t, eng, dir, 1, 0, []string{"up"}, []string{"old"}, 1)
	if err := eng.Put(ctx, []byte("up"), []byte("new")); err != nil {
		t.Fatalf("Put failed: %v", err)
	}
	if got, _ := eng.Get([]byte("up")); !bytes.Equal(got, []byte("new")) {
		t.Fatalf("expected new, got %q", got)
	}
}

func TestEngineM01_DeletePersistedKey(t *testing.T) {
	eng, dir := newDiskEngine(t, 256)
	defer func() { _ = eng.Close() }()
	ctx := testCtx()

	buildSSTable(t, eng, dir, 1, 0, []string{"del"}, []string{"old"}, 1)
	if err := eng.Delete(ctx, []byte("del")); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	if _, err := eng.Get([]byte("del")); !stdErrors.Is(err, errors.ErrKeyNotFound) {
		t.Fatalf("expected NotFound after delete of persisted key, got %v", err)
	}
}

func TestEngineM01_L0NewestWins(t *testing.T) {
	eng, dir := newDiskEngine(t, 256)
	defer func() { _ = eng.Close() }()

	// Two overlapping L0 files: older fileNum=1 has V1, newer fileNum=2 has V2.
	buildSSTable(t, eng, dir, 1, 0, []string{"dup"}, []string{"V1"}, 1)
	buildSSTable(t, eng, dir, 2, 0, []string{"dup"}, []string{"V2"}, 100)

	got, err := eng.Get([]byte("dup"))
	if err != nil {
		t.Fatalf("Get dup failed: %v", err)
	}
	if !bytes.Equal(got, []byte("V2")) {
		t.Fatalf("L0 newest wins: got %q want V2", got)
	}
}

func TestEngineM01_BlockCacheIntegration(t *testing.T) {
	eng, dir := newDiskEngine(t, 256)
	defer func() { _ = eng.Close() }()

	buildSSTable(t, eng, dir, 5, 0, []string{"cached"}, []string{"cv"}, 1)
	bc := eng.BlockCache()
	if bc == nil {
		t.Fatalf("expected shared block cache from Open")
	}
	before := bc.Len()
	if _, err := eng.Get([]byte("cached")); err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	after := bc.Len()
	if after <= before {
		t.Fatalf("expected block cache to populate after persistent Get (before=%d after=%d)", before, after)
	}
	// Second Get must still return correct value (hit path).
	got, err := eng.Get([]byte("cached"))
	if err != nil || !bytes.Equal(got, []byte("cv")) {
		t.Fatalf("second Get failed: %q %v", got, err)
	}
}

func TestEngineM01_NotFoundVsCorruption(t *testing.T) {
	eng, dir := newDiskEngine(t, 64)
	defer func() { _ = eng.Close() }()
	ctx := testCtx()

	// Valid persisted key for control.
	buildSSTable(t, eng, dir, 1, 0, []string{"ok"}, []string{"v"}, 1)
	if _, err := eng.Get([]byte("ok")); err != nil {
		t.Fatalf("control Get failed: %v", err)
	}
	// Genuine miss => ErrKeyNotFound.
	if _, err := eng.Get([]byte("absent")); !stdErrors.Is(err, errors.ErrKeyNotFound) {
		t.Fatalf("expected NotFound, got %v", err)
	}
	// Corrupt the SSTable file: Get must return a storage error, never NotFound.
	_ = ctx
	vs := eng.VersionSet()
	ver := vs.Current()
	if ver == nil {
		t.Fatalf("no current version")
	}
	files := ver.Files(0)
	ver.Unref()
	if len(files) == 0 {
		t.Fatalf("no L0 files")
	}
	path := version.TablePath(dir, files[0].FileNum)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read sstable failed: %v", err)
	}
	// Flip a byte in the first data block (offset 0) to break CRC.
	data[0] ^= 0xff
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatalf("corrupt write failed: %v", err)
	}
	// Clear cache so the corrupted bytes are actually read from disk.
	if bc := eng.BlockCache(); bc != nil {
		bc.Clear()
	}
	_, err = eng.Get([]byte("ok"))
	if err == nil {
		t.Fatalf("expected corruption error, got success")
	}
	if stdErrors.Is(err, errors.ErrKeyNotFound) {
		t.Fatalf("corruption must not map to NotFound, got %v", err)
	}
}
