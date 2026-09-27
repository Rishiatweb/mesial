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

// TestMergeIgnoreDoesNotAliasSpareCapacity actually reproduces the aliasing
// hazard instead of relying on DefaultIgnore's current len==cap declaration
// (which makes append(DefaultIgnore, userIgnore...) always reallocate, so a
// test against the real DefaultIgnore can't distinguish the fixed
// implementation from the buggy one). It swaps DefaultIgnore for a
// same-content slice with deliberate spare capacity, plants a sentinel just
// past its length in the shared backing array, and asserts MergeIgnore never
// overwrites it — the exact corruption append(DefaultIgnore, userIgnore...)
// would cause if DefaultIgnore ever gains spare capacity for real.
func TestMergeIgnoreDoesNotAliasSpareCapacity(t *testing.T) {
	saved := DefaultIgnore
	t.Cleanup(func() { DefaultIgnore = saved })

	backing := make([]string, len(saved), len(saved)+4)
	copy(backing, saved)
	full := backing[:cap(backing)] // exposes the spare capacity for inspection
	const sentinel = "__SENTINEL__"
	full[len(saved)] = sentinel

	DefaultIgnore = backing[:len(saved)] // same content, but now has spare capacity

	_ = MergeIgnore([]string{"a", "b", "c"})

	if got := full[len(saved)]; got != sentinel {
		t.Fatalf("MergeIgnore wrote into DefaultIgnore's spare capacity: slot past len = %q, want untouched %q", got, sentinel)
	}
}
