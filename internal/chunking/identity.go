package chunking

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

// ComputeAnchorID returns a deterministic, content-independent identity key
// for a chunk: sha256(sourcePath + "\x00" + breadcrumb), hex-encoded.
//
// This is computed ONCE, when a chunk is first created, from the breadcrumb
// it had at that moment — and then carried forward on every subsequent
// ingest. It is never recomputed from a chunk's current breadcrumb to
// "re-derive" identity; that would defeat the point (a renamed heading would
// silently become a different identity). Callers compute a candidate
// anchor ID for each NEW chunk and match it against previously-stored
// anchor IDs to decide whether this is the same chunk, a moved (renamed)
// chunk, or a genuinely new one.
func ComputeAnchorID(sourcePath, breadcrumb string) string {
	h := sha256.Sum256([]byte(sourcePath + "\x00" + breadcrumb))
	return hex.EncodeToString(h[:])
}

// ComputeContentHash returns sha256(normalizeContent(content)), hex-encoded.
// Used both to detect whether a chunk's content actually changed (skip the
// write entirely if not) and, as a fallback identity signal, to recognize a
// chunk whose content is unchanged but whose heading (and therefore anchor
// ID) moved.
func ComputeContentHash(content string) string {
	h := sha256.Sum256([]byte(normalizeContent(content)))
	return hex.EncodeToString(h[:])
}

var wsRunRe = regexp.MustCompile(`\s+`)

// normalizeContent collapses all whitespace runs (including newlines) to a
// single space and trims the result, so trivial reformatting (rewrapped
// lines, added blank lines, trailing whitespace) doesn't register as a
// content change.
//
// Deliberately conservative: this does not normalize case or Markdown
// syntax variants (e.g. "- " vs "* " bullets, "**bold**" vs "__bold__").
// Folding those too would make two genuinely different-looking chunks
// collapse into "unchanged" by accident. Revisit only if false-"unchanged"
// reports show up in practice.
func normalizeContent(content string) string {
	return strings.TrimSpace(wsRunRe.ReplaceAllString(content, " "))
}
