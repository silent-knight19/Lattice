package version_test

import (
	"math"
	"testing"

	"github.com/silent-knight19/lattice/internal/version"
)

// TestINDM001_RefCyclingNeverWraps replays the IND-M-001 scenario at scale:
// sustained rapid Ref/Unref cycling must keep the count exactly balanced and
// never wrap into negative territory (which would strand the version by
// disabling finalize).
func TestINDM001_RefCyclingNeverWraps(t *testing.T) {
	v := version.NewVersion([version.NumLevels][]version.FileMetadata{})
	const cycles = 100_000
	for i := 0; i < cycles; i++ {
		v.Ref()
		v.Unref()
	}
	if got := v.RefCount(); got != 1 {
		t.Fatalf("refCount after %d balanced cycles = %d, want 1 (drift or wrap)", cycles, got)
	}
	if got := v.RefCount(); got <= 0 {
		t.Fatalf("refCount went non-positive: %d (IND-M-001 wrap)", got)
	}
}

// TestINDM001_CapRejectsWithoutWrapping proves the upper boundary is
// fail-closed, not wrapping: MaxInt64-1 → Ref → MaxInt64 → further Ref panics
// and TryRef refuses, while Unref keeps the version live. A wrap to MinInt64
// would report a negative live count and break finalize; it must be
// unreachable.
func TestINDM001_CapRejectsWithoutWrapping(t *testing.T) {
	v := version.NewVersion([version.NumLevels][]version.FileMetadata{})
	v.SetRefCountForTesting(math.MaxInt64 - 1)

	v.Ref()
	if got := v.RefCount(); got != math.MaxInt64 {
		t.Fatalf("refCount = %d, want MaxInt64", got)
	}

	func() {
		defer func() {
			if recover() == nil {
				t.Errorf("Ref() at MaxInt64 did not panic (must reject, not wrap)")
			}
		}()
		v.Ref()
	}()
	if got := v.RefCount(); got != math.MaxInt64 {
		t.Errorf("refCount mutated by rejected Ref: %d", got)
	}
	if v.TryRef() {
		t.Errorf("TryRef() at MaxInt64 succeeded (must refuse, not wrap)")
	}
	if got := v.RefCount(); got <= 0 {
		t.Errorf("refCount non-positive at cap: %d (wrap!)", got)
	}

	v.Unref()
	if got := v.RefCount(); got != math.MaxInt64-1 {
		t.Errorf("refCount after Unref from cap = %d, want MaxInt64-1 (still live)", got)
	}
}
