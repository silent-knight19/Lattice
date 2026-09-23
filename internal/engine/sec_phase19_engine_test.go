package engine_test

import (
	"path/filepath"
	"testing"

	"github.com/silent-knight19/lattice/internal/engine"
)

// TestPhase19_Engine_InvalidDBPathFailsClosed verifies that NewEngineWithOptions
// fails closed when given an invalid or escaping DBPath.
func TestPhase19_Engine_InvalidDBPathFailsClosed(t *testing.T) {
	dir := t.TempDir()

	invalidPaths := []string{
		filepath.Join(dir, "..\x00bad"),
		filepath.Join(dir, "bad\x00path"),
	}

	for _, p := range invalidPaths {
		eng := engine.NewEngineWithOptions(engine.EngineOptions{
			DBPath:       p,
			Backpressure: engine.DefaultBackpressureConfig(),
		})
		if err := eng.Open(); err == nil {
			_ = eng.Close()
			t.Errorf("expected Open() to fail for invalid DBPath %q, got nil", p)
		}
	}
}
