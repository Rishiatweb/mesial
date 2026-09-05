package chunking

import "testing"

func TestComputeContentHash(t *testing.T) {
	t.Run("whitespace differences don't change the hash", func(t *testing.T) {
		a := ComputeContentHash("Hello   world.\n\nSecond line.")
		b := ComputeContentHash("Hello world.\nSecond line.")
		if a != b {
			t.Errorf("expected same hash for whitespace-only difference, got %q vs %q", a, b)
		}
	})

	t.Run("leading/trailing whitespace is trimmed", func(t *testing.T) {
		a := ComputeContentHash("  content  ")
		b := ComputeContentHash("content")
		if a != b {
			t.Errorf("expected same hash after trim, got %q vs %q", a, b)
		}
	})

	t.Run("different content produces different hash", func(t *testing.T) {
		a := ComputeContentHash("Hello world.")
		b := ComputeContentHash("Goodbye world.")
		if a == b {
			t.Errorf("expected different hashes for different content, both %q", a)
		}
	})

	t.Run("case is NOT normalized (deliberate v1 limitation)", func(t *testing.T) {
		a := ComputeContentHash("Hello World")
		b := ComputeContentHash("hello world")
		if a == b {
			t.Errorf("case folding is not implemented in v1; if this now matches, normalizeContent's documented limitation changed and the comment should be updated")
		}
	})

	t.Run("empty content does not panic", func(t *testing.T) {
		got := ComputeContentHash("")
		if got == "" {
			t.Error("expected a non-empty hash even for empty content")
		}
	})
}

func TestComputeAnchorID(t *testing.T) {
	t.Run("same source and breadcrumb produce the same anchor ID", func(t *testing.T) {
		a := ComputeAnchorID("/repo/docs/README.md", "Overview > Setup")
		b := ComputeAnchorID("/repo/docs/README.md", "Overview > Setup")
		if a != b {
			t.Errorf("expected identical anchor IDs for identical inputs, got %q vs %q", a, b)
		}
	})

	t.Run("different breadcrumb produces a different anchor ID", func(t *testing.T) {
		a := ComputeAnchorID("/repo/docs/README.md", "Overview > Setup")
		b := ComputeAnchorID("/repo/docs/README.md", "Overview > Install")
		if a == b {
			t.Errorf("expected different anchor IDs for different breadcrumbs, both %q", a)
		}
	})

	t.Run("different source produces a different anchor ID", func(t *testing.T) {
		a := ComputeAnchorID("/repo/docs/README.md", "Overview > Setup")
		b := ComputeAnchorID("/repo/docs/OTHER.md", "Overview > Setup")
		if a == b {
			t.Errorf("expected different anchor IDs for different sources, both %q", a)
		}
	})

	t.Run("empty source or breadcrumb does not panic", func(t *testing.T) {
		got := ComputeAnchorID("", "")
		if got == "" {
			t.Error("expected a non-empty anchor ID even for empty inputs")
		}
	})

	t.Run("source/breadcrumb boundary is not confusable (no delimiter injection)", func(t *testing.T) {
		// "ab" + "c" must not collide with "a" + "bc" -- the \x00 separator
		// in ComputeAnchorID's implementation is what prevents this; assert
		// the observable property rather than the implementation detail.
		a := ComputeAnchorID("ab", "c")
		b := ComputeAnchorID("a", "bc")
		if a == b {
			t.Error("expected source+breadcrumb concatenation to not collide across the boundary")
		}
	})
}
