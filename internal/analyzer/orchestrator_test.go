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
