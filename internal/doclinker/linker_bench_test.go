package doclinker

import (
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"testing"
)

// This file measures the doclinker's actual scaling behavior, independent of
// FalkorDB — the two DB-fetching calls in link() (FetchCodeEntities,
// FetchChunks) aren't the interesting cost; the matching loop after them is.
//
// RATIONALE.md's "what's deliberately not built" table describes the current
// approach as an "O(chunks × names) scan" and says it's "fine to ~10k × 10k."
// That's not what the code does: nameToIDs is a map, so match cost is O(1)
// average per candidate token regardless of how many names are registered.
// The real cost driver is candidate-token volume (proportional to total
// chunk content length), not the entity-name count. These benchmarks measure
// that directly, holding chunk content fixed while scaling the name count,
// and vice versa.

// syntheticEntityNames generates n plausible multi-segment identifiers (the
// shape isLinkableBare requires) so nameToIDs population and candidate
// extraction both do realistic work, not just short string comparisons.
func syntheticEntityNames(n int) []string {
	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprintf("EntityHandler%d", i)
	}
	return names
}

// syntheticChunkContent builds one chunk's worth of prose mixing real
// identifier mentions (a sample of `hitNames`, so some candidates resolve)
// with filler prose (so most candidates don't) and code-block noise (so the
// triple-fence strip isn't a no-op). Roughly mirrors a real markdown section.
func syntheticChunkContent(rng *rand.Rand, hitNames []string, wordsPerChunk int) string {
	var sb strings.Builder
	fillerWords := []string{"the", "system", "uses", "a", "pattern", "to", "handle", "requests", "and", "return", "results", "for", "callers", "in", "this", "module"}
	for i := 0; i < wordsPerChunk; i++ {
		if i > 0 {
			sb.WriteByte(' ')
		}
		if i%7 == 0 && len(hitNames) > 0 {
			sb.WriteString(hitNames[rng.Intn(len(hitNames))])
		} else if i%11 == 0 {
			sb.WriteString("`SomeBareToken" + strconv.Itoa(rng.Intn(1000)) + "`")
		} else {
			sb.WriteString(fillerWords[rng.Intn(len(fillerWords))])
		}
	}
	// One fenced code block per chunk, containing identifiers that must NOT
	// leak into bare matches (exercises the strip path, not just plain scan).
	sb.WriteString("\n```ts\nconst x = new EntityHandler0();\nx.process();\n```\n")
	return sb.String()
}

// runMatchLoop replicates link()'s post-fetch logic exactly (build map, scan
// chunks, look up candidates) without any store/DB involvement.
func runMatchLoop(scanner *chunkScanner, nameToIDs map[string][]int64, chunkContents []string) int {
	edges := 0
	for _, content := range chunkContents {
		for _, tok := range scanner.candidates(content, false) {
			ids, ok := nameToIDs[tok]
			if !ok {
				continue
			}
			edges += len(ids)
		}
	}
	return edges
}

func buildNameToIDs(names []string) map[string][]int64 {
	m := make(map[string][]int64, len(names))
	for i, n := range names {
		m[n] = append(m[n], int64(i))
	}
	return m
}

// BenchmarkMatchLoop_ScaleNames holds chunk count and content fixed (200
// chunks, 300 words each) and scales only the registered entity-name count.
// If the claimed "O(chunks × names)" were accurate, runtime here should grow
// roughly linearly with nameCount. Because nameToIDs is a map, it should not.
func BenchmarkMatchLoop_ScaleNames(b *testing.B) {
	const chunkCount = 200
	const wordsPerChunk = 300
	for _, nameCount := range []int{100, 1_000, 10_000, 100_000} {
		nameCount := nameCount
		b.Run(fmt.Sprintf("names=%d", nameCount), func(b *testing.B) {
			rng := rand.New(rand.NewSource(42))
			names := syntheticEntityNames(nameCount)
			nameToIDs := buildNameToIDs(names)
			hitSample := names[:min(50, len(names))]
			contents := make([]string, chunkCount)
			for i := range contents {
				contents[i] = syntheticChunkContent(rng, hitSample, wordsPerChunk)
			}
			scanner := newScanner()

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				runMatchLoop(scanner, nameToIDs, contents)
			}
		})
	}
}

// BenchmarkMatchLoop_ScaleChunks holds the name count fixed (10,000 — the
// upper bound RATIONALE.md's "fine to ~10k" claim names) and scales chunk
// count, the dimension that actually should drive cost under a map-based
// design.
func BenchmarkMatchLoop_ScaleChunks(b *testing.B) {
	const nameCount = 10_000
	const wordsPerChunk = 300
	for _, chunkCount := range []int{200, 2_000, 20_000} {
		chunkCount := chunkCount
		b.Run(fmt.Sprintf("chunks=%d", chunkCount), func(b *testing.B) {
			rng := rand.New(rand.NewSource(42))
			names := syntheticEntityNames(nameCount)
			nameToIDs := buildNameToIDs(names)
			hitSample := names[:50]
			contents := make([]string, chunkCount)
			for i := range contents {
				contents[i] = syntheticChunkContent(rng, hitSample, wordsPerChunk)
			}
			scanner := newScanner()

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				runMatchLoop(scanner, nameToIDs, contents)
			}
		})
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
