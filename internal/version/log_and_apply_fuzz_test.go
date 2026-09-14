package version

import (
	"path/filepath"
	"testing"
)

func FuzzLogAndApply(f *testing.F) {
	// Seed corpus: empty edit
	eEmpty := NewVersionEdit()
	f.Add(eEmpty.Encode())

	// Seed corpus: scalar edit
	eScalar := NewVersionEdit()
	eScalar.SetNextFileNum(100)
	eScalar.SetLastSeqNum(50)
	f.Add(eScalar.Encode())

	// Seed corpus: valid add file
	eAdd := NewVersionEdit()
	eAdd.SetNextFileNum(10)
	eAdd.SetLastSeqNum(20)
	_ = eAdd.AddFile(0, makeTestFileMeta(1, 1024, "a", "b", 1, 10))
	f.Add(eAdd.Encode())

	// Seed corpus: delete file
	eDel := NewVersionEdit()
	_ = eDel.DeleteFile(0, 1)
	f.Add(eDel.Encode())

	f.Fuzz(func(t *testing.T, data []byte) {
		edit, err := DecodeVersionEdit(data)
		if err != nil {
			return // malformed wire framing: ignored
		}

		dir := t.TempDir()
		manPath := filepath.Join(dir, ManifestFilename(1))
		w, err := CreateManifestWriter(manPath)
		if err != nil {
			t.Fatalf("failed to create manifest writer: %v", err)
		}
		defer func() { _ = w.Close() }()

		// Notice: dbPath is empty string so physical disk checks are bypassed during pure fuzzing
		vs := NewVersionSetWithOptions(VersionSetOptions{
			DBPath:         "",
			ManifestWriter: w,
			NextFileNum:    1,
			LastSeqNum:     0,
		})

		// Must not panic, hang, or leave inconsistent state
		applyErr := vs.LogAndApply(edit)
		if applyErr == nil {
			cur := vs.Current()
			if cur == nil {
				t.Fatalf("expected non-nil Current upon successful LogAndApply")
			}
			defer cur.Unref()

			// Verify L0 sorted by FileNum ASC
			l0 := cur.Files(0)
			for i := 0; i < len(l0)-1; i++ {
				if l0[i].FileNum >= l0[i+1].FileNum {
					t.Fatalf("L0 files not sorted by FileNum ASC: %d >= %d", l0[i].FileNum, l0[i+1].FileNum)
				}
			}

			// Verify L1..L6 non-overlapping
			for lvl := 1; lvl < NumLevels; lvl++ {
				files := cur.Files(lvl)
				for i := 0; i < len(files)-1; i++ {
					ikA := decodeInternalKeyNoAlloc(files[i].LargestKey)
					ikB := decodeInternalKeyNoAlloc(files[i+1].SmallestKey)
					if ikA.Compare(ikB) >= 0 {
						t.Fatalf("level %d files overlap: file %d vs file %d", lvl, files[i].FileNum, files[i+1].FileNum)
					}
				}
			}
		}
	})
}
