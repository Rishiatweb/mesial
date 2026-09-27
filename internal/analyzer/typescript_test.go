package analyzer

import "testing"

func TestIsDependencyRecognizesAllDefaultIgnoreEntries(t *testing.T) {
	a := &TypeScriptAnalyzer{}
	for _, dir := range DefaultIgnore {
		path := "/repo/" + dir + "/lib/index.ts"
		if !a.IsDependency(path) {
			t.Errorf("IsDependency(%q) = false, want true (dir %q is in DefaultIgnore)", path, dir)
		}
	}
}

func TestIsDependencyIgnoresUnrelatedPaths(t *testing.T) {
	a := &TypeScriptAnalyzer{}
	path := "/repo/src/index.ts"
	if a.IsDependency(path) {
		t.Errorf("IsDependency(%q) = true, want false", path)
	}
}
