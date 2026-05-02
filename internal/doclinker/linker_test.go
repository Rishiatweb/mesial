package doclinker

import "testing"

func TestIsLinkableBare(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		// Multi-segment identifiers — link.
		{"EventViewImpl", true},
		{"getPullRequest", true},
		{"get_pull_request", true},
		{"XMLParser", true},
		{"MAX_RETRY", true},
		{"JsonParser", true},

		// Single-segment / common words — don't link bare.
		{"User", false},
		{"Configuration", false},
		{"init", false},
		{"JSON", false},

		// Length floor.
		{"id", false},
		{"AB", false},
		{"OK", false},

		// Underscore but only one non-empty segment.
		{"User_", false},
		{"_internal", false},
		{"__init__", false},
	}
	for _, tc := range cases {
		got := isLinkableBare(tc.in)
		if got != tc.want {
			t.Errorf("isLinkableBare(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestScannerCandidates(t *testing.T) {
	s := newScanner()

	t.Run("backtick tokens always link", func(t *testing.T) {
		got := s.candidates("see `User` and `init` for setup", false)
		if !contains(got, "User") || !contains(got, "init") {
			t.Errorf("expected User and init via backticks, got %v", got)
		}
	})

	t.Run("bare tokens require multi-segment", func(t *testing.T) {
		got := s.candidates("EventViewImpl extends User and calls getPullRequest", false)
		if !contains(got, "EventViewImpl") || !contains(got, "getPullRequest") {
			t.Errorf("expected multi-segment bare tokens, got %v", got)
		}
		if contains(got, "User") {
			t.Errorf("did not expect bare User, got %v", got)
		}
	})

	t.Run("strict mode skips bare", func(t *testing.T) {
		got := s.candidates("EventViewImpl is a class", true)
		if contains(got, "EventViewImpl") {
			t.Errorf("strict mode should skip bare tokens, got %v", got)
		}
	})

	t.Run("triple-fence excluded from bare", func(t *testing.T) {
		content := "Here is a snippet:\n```ts\nconst foo = new EventViewImpl();\n```\nThe end."
		got := s.candidates(content, false)
		if contains(got, "EventViewImpl") {
			t.Errorf("identifiers inside triple-fence blocks should not link bare, got %v", got)
		}
	})

	t.Run("triple-fence still strips when strict", func(t *testing.T) {
		content := "```\nEventViewImpl\n```"
		got := s.candidates(content, true)
		if len(got) != 0 {
			t.Errorf("strict + triple-fence should yield no candidates, got %v", got)
		}
	})

	t.Run("dedup across backtick and bare", func(t *testing.T) {
		got := s.candidates("`getPullRequest` and getPullRequest again", false)
		count := 0
		for _, tok := range got {
			if tok == "getPullRequest" {
				count++
			}
		}
		if count != 1 {
			t.Errorf("expected one occurrence of getPullRequest, got %d (full: %v)", count, got)
		}
	})
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
