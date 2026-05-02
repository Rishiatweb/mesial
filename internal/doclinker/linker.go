// Package doclinker links doc chunks to code entities by scanning chunk content
// for identifier mentions and emitting (:Chunk)-[:DOCUMENTS]->(:CodeEntity) edges.
package doclinker

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/mknw/h9s/internal/falkorstore"
)

// Linker emits DOCUMENTS edges from chunks to code entities they mention.
type Linker struct {
	store   *falkorstore.Store
	scanner *chunkScanner
}

// New constructs a Linker over the given store.
func New(store *falkorstore.Store) *Linker {
	return &Linker{store: store, scanner: newScanner()}
}

// LinkRepo scans every chunk in the graph and emits DOCUMENTS edges for each
// identifier mention that resolves to a code entity. Idempotent (MERGE).
func (l *Linker) LinkRepo(ctx context.Context, strict bool) (int, error) {
	return l.link(ctx, "", strict)
}

// LinkBySource scans only chunks with c.source == source. Use this after
// re-ingesting a single doc file.
func (l *Linker) LinkBySource(ctx context.Context, source string, strict bool) (int, error) {
	return l.link(ctx, source, strict)
}

func (l *Linker) link(ctx context.Context, source string, strict bool) (int, error) {
	entities, err := l.store.FetchCodeEntities(ctx)
	if err != nil {
		return 0, fmt.Errorf("fetching code entities: %w", err)
	}
	if len(entities) == 0 {
		return 0, nil
	}
	nameToIDs := make(map[string][]int64, len(entities))
	for _, e := range entities {
		nameToIDs[e.Name] = append(nameToIDs[e.Name], e.ID)
	}

	chunks, err := l.store.FetchChunks(ctx, source)
	if err != nil {
		return 0, fmt.Errorf("fetching chunks: %w", err)
	}

	edges := 0
	for _, c := range chunks {
		for _, tok := range l.scanner.candidates(c.Content, strict) {
			ids, ok := nameToIDs[tok]
			if !ok {
				continue
			}
			for _, targetID := range ids {
				if err := l.store.ConnectEntities(ctx, "DOCUMENTS", c.ID, targetID, nil); err != nil {
					return edges, fmt.Errorf("linking chunk %d -> entity %d: %w", c.ID, targetID, err)
				}
				edges++
			}
		}
	}
	return edges, nil
}

// chunkScanner extracts candidate identifier tokens from a chunk's content.
type chunkScanner struct {
	inlineBacktick *regexp.Regexp
	tripleFence    *regexp.Regexp
	identToken     *regexp.Regexp
}

func newScanner() *chunkScanner {
	return &chunkScanner{
		inlineBacktick: regexp.MustCompile("`([^`\n]+)`"),
		tripleFence:    regexp.MustCompile("(?s)```.*?```"),
		identToken:     regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`),
	}
}

// candidates returns deduplicated tokens worth looking up against the name map.
// Backtick-fenced tokens always pass; bare tokens require isLinkableBare. In
// strict mode, bare tokens are skipped entirely.
func (s *chunkScanner) candidates(content string, strict bool) []string {
	var out []string
	seen := make(map[string]bool)

	// Drop triple-fenced code blocks so identifiers inside code samples don't
	// produce links via the bare path.
	stripped := s.tripleFence.ReplaceAllString(content, "\n")

	for _, m := range s.inlineBacktick.FindAllStringSubmatch(stripped, -1) {
		tok := strings.TrimSpace(m[1])
		if tok == "" || seen[tok] {
			continue
		}
		seen[tok] = true
		out = append(out, tok)
	}

	if strict {
		return out
	}

	bare := s.inlineBacktick.ReplaceAllString(stripped, " ")
	for _, m := range s.identToken.FindAllString(bare, -1) {
		if seen[m] || !isLinkableBare(m) {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	return out
}

// isLinkableBare encodes the bare-token rule: length >= 4 AND has at least
// two case segments. Two-segment forms accepted: PascalCase with internal
// case boundary (EventViewImpl, XMLParser), camelCase (getPullRequest), and
// snake_case with >= 2 non-empty parts (get_pull_request, MAX_RETRY).
// Excludes single-segment words (User, Configuration, init, JSON).
func isLinkableBare(s string) bool {
	if len(s) < 4 {
		return false
	}
	if strings.Contains(s, "_") {
		nonEmpty := 0
		for _, part := range strings.Split(s, "_") {
			if part != "" {
				nonEmpty++
			}
		}
		return nonEmpty >= 2
	}
	return hasInternalCaseBoundary(s)
}

// hasInternalCaseBoundary reports whether s has an internal lower→upper
// transition (camelCase / PascalCase boundary) or an upper-run followed by a
// lowercase letter (XMLParser). Operates on bytes — identifiers are ASCII.
func hasInternalCaseBoundary(s string) bool {
	for i := 1; i < len(s); i++ {
		if isLower(s[i-1]) && isUpper(s[i]) {
			return true
		}
	}
	for i := 2; i < len(s); i++ {
		if isUpper(s[i-2]) && isUpper(s[i-1]) && isLower(s[i]) {
			return true
		}
	}
	return false
}

func isLower(c byte) bool { return c >= 'a' && c <= 'z' }
func isUpper(c byte) bool { return c >= 'A' && c <= 'Z' }
