package analyzer

import "testing"

func TestMergeIgnoreCombinesDefaultsAndUserEntries(t *testing.T) {
	got := MergeIgnore([]string{"generated", "node_modules"})
	seen := make(map[string]int, len(got))
	for _, name := range got {
		seen[name]++
	}
	for _, name := range DefaultIgnore {
		if seen[name] != 1 {
			t.Fatalf("default ignore %q count = %d, want 1", name, seen[name])
		}
	}
	if seen["generated"] != 1 {
		t.Fatalf("custom ignore count = %d, want 1", seen["generated"])
	}
}

// TestMergeIgnoreDoesNotMutateDefaultIgnore guards against the aliasing risk
// in append(DefaultIgnore, userIgnore...): if DefaultIgnore is ever declared
// with spare capacity, that pattern would write userIgnore's entries directly
// into DefaultIgnore's backing array. Calling MergeIgnore repeatedly with
// different user lists must never change DefaultIgnore's contents or length.
func TestMergeIgnoreDoesNotMutateDefaultIgnore(t *testing.T) {
	before := append([]string(nil), DefaultIgnore...)

	_ = MergeIgnore([]string{"one"})
	_ = MergeIgnore([]string{"two", "three"})
	_ = MergeIgnore(nil)

	if len(DefaultIgnore) != len(before) {
		t.Fatalf("DefaultIgnore length changed: got %d, want %d", len(DefaultIgnore), len(before))
	}
	for i, name := range before {
		if DefaultIgnore[i] != name {
			t.Fatalf("DefaultIgnore[%d] = %q, want %q (DefaultIgnore was mutated)", i, DefaultIgnore[i], name)
		}
	}
}
