package version

import (
	"testing"
)

func BenchmarkVersion_RefUnref(b *testing.B) {
	v := NewVersion([NumLevels][]FileMetadata{})
	defer v.Unref() // creator ref

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		v.Ref()
		v.Unref()
	}
}

func BenchmarkVersionSet_AppendVersion(b *testing.B) {
	vs := NewVersionSet()

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		v := NewVersion([NumLevels][]FileMetadata{})
		if err := vs.AppendVersion(v); err != nil {
			b.Fatalf("AppendVersion failed: %v", err)
		}
	}
}

func BenchmarkVersionSet_Current(b *testing.B) {
	vs := NewVersionSet()
	v := NewVersion([NumLevels][]FileMetadata{})
	if err := vs.AppendVersion(v); err != nil {
		b.Fatalf("AppendVersion failed: %v", err)
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		cur := vs.Current()
		cur.Unref()
	}
}
