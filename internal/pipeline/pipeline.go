// Package pipeline holds the orchestration logic shared by both the MCP server
// (cmd/ingest) and the dev CLI (cmd/h9s-cli). It composes lower-level packages
// (chunking, embedding, falkorstore, analyzer, doclinker) into the operations
// users actually request: ingest docs, analyze a repository, search, link.
package pipeline

import (
	"context"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mknw/h9s/internal/analyzer"
	"github.com/mknw/h9s/internal/chunking"
	"github.com/mknw/h9s/internal/doclinker"
	"github.com/mknw/h9s/internal/embedding"
	"github.com/mknw/h9s/internal/falkorstore"
)

// EmbeddingDim is the dimensionality used everywhere in the project (Qwen3
// native 1024, Matryoshka-truncated to 512).
const EmbeddingDim = 512

// OversizedChunkChars is the per-chunk char ceiling above which we skip
// embedding (would dilute the vector and risks blowing the embedding model's
// context). Oversized chunks are still stored with full content, OF_FILE, and
// oversized=true so the linker scans them and graph traversal sees them.
const OversizedChunkChars = 6000

// IngestResult summarizes a document ingestion run.
//
// ChunksStored keeps its pre-Increment-1 meaning (created + updated +
// renamed — i.e. every chunk actually touched) so existing callers reading
// this field don't need to change. Created/Updated/Renamed/Unchanged/
// Orphaned are additive, giving visibility into what match-in-place
// ingestion actually did without requiring a separate `reanchor` call for
// the common case.
type IngestResult struct {
	ChunksStored    int
	OversizedChunks int
	EdgesAsserted   int
	Created         int
	Updated         int
	Renamed         int
	Unchanged       int
	Orphaned        int
}

// SourceIngestReport classifies what happened to every chunk of one source
// file during a match-first re-ingest pass.
type SourceIngestReport struct {
	Source          string
	Created         []int64
	Updated         []int64
	Renamed         []int64
	Unchanged       []int64
	Orphaned        []int64
	OversizedChunks int
	EdgesLinked     int
}

// AnalyzeResult summarizes a full analyze_repository run (code + docs + link).
// The caller knows which graph it opened the store against, so the result does
// not duplicate that name.
type AnalyzeResult struct {
	Files           int
	Entities        int
	Relationships   int
	AnalyzeErrors   int
	DocChunks       int
	OversizedChunks int
	DocEdges        int
}

// ResolveRepoGraphName picks the FalkorDB graph name for a repo-scoped
// operation. Explicit name wins; otherwise walk up from anyPath looking for a
// .git directory and use filepath.Base of the directory containing it. Errors
// if neither resolves.
func ResolveRepoGraphName(explicitRepo, anyPath string) (string, error) {
	if explicitRepo != "" {
		return explicitRepo, nil
	}
	if anyPath == "" {
		return "", fmt.Errorf("no repo name and no path provided to resolve repo from")
	}
	p, err := filepath.Abs(anyPath)
	if err != nil {
		return "", fmt.Errorf("resolving abs path: %w", err)
	}
	if info, err := os.Stat(p); err == nil && !info.IsDir() {
		p = filepath.Dir(p)
	}
	for {
		gitPath := filepath.Join(p, ".git")
		if info, err := os.Stat(gitPath); err == nil && info.IsDir() {
			return filepath.Base(p), nil
		}
		parent := filepath.Dir(p)
		if parent == p {
			return "", fmt.Errorf("no .git directory found walking up from %q", anyPath)
		}
		p = parent
	}
}

// ResolvePathsToRepo confirms multiple paths resolve to a single repo when no
// explicit repo is given. Returns the resolved name or an error if paths span
// different repos.
func ResolvePathsToRepo(explicitRepo string, paths []string) (string, error) {
	if explicitRepo != "" {
		return explicitRepo, nil
	}
	if len(paths) == 0 {
		return "", fmt.Errorf("no paths provided")
	}
	name, err := ResolveRepoGraphName("", paths[0])
	if err != nil {
		return "", err
	}
	for _, p := range paths[1:] {
		other, err := ResolveRepoGraphName("", p)
		if err != nil {
			return "", fmt.Errorf("resolving repo for %s: %w", p, err)
		}
		if other != name {
			return "", fmt.Errorf("paths span multiple repos: %q and %q", name, other)
		}
	}
	return name, nil
}

// IngestDocs expands the given paths to .md files (skipping `ignore`d
// directories), then per source file: MERGEs a :File node, deletes existing
// chunks for that source, chunks + embeds + stores new chunks anchored via
// :OF_FILE, and runs the doc linker (LinkBySource). Chunks longer than
// OversizedChunkChars are stored without a vector but still linked.
func IngestDocs(ctx context.Context, store *falkorstore.Store, embedder *embedding.Client, paths, ignore []string, strict bool) (IngestResult, error) {
	ignoreSet := make(map[string]bool, len(ignore))
	for _, name := range ignore {
		ignoreSet[name] = true
	}

	var files []string
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return IngestResult{}, fmt.Errorf("stat %s: %w", p, err)
		}
		if info.IsDir() {
			err := filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if d.IsDir() {
					if ignoreSet[d.Name()] {
						return filepath.SkipDir
					}
					return nil
				}
				if strings.HasSuffix(strings.ToLower(d.Name()), ".md") {
					files = append(files, path)
				}
				return nil
			})
			if err != nil {
				return IngestResult{}, fmt.Errorf("scanning %s: %w", p, err)
			}
		} else if strings.HasSuffix(strings.ToLower(p), ".md") {
			files = append(files, p)
		}
	}

	var res IngestResult
	for _, f := range files {
		report, err := reingestSource(ctx, store, embedder, f, strict)
		if err != nil {
			return res, err
		}
		if report == nil {
			continue // empty file, nothing to do
		}
		res.Created += len(report.Created)
		res.Updated += len(report.Updated)
		res.Renamed += len(report.Renamed)
		res.Unchanged += len(report.Unchanged)
		res.Orphaned += len(report.Orphaned)
		res.OversizedChunks += report.OversizedChunks
		res.ChunksStored += len(report.Created) + len(report.Updated) + len(report.Renamed)
		res.EdgesAsserted += report.EdgesLinked
	}

	return res, nil
}

// reingestSource chunks one markdown file, resolves every resulting chunk
// against the source's existing identity state (match-in-place — see
// docs/DESIGN.md's stable-identity note), embeds only what actually needs
// it, marks anything left over as orphaned, and re-runs the linker. This is
// the single implementation both IngestDocs and a future `reanchor` (see
// docs/TIER1_CONTINUATION.md) call — the classification logic exists here
// exactly once.
//
// Returns nil (no error) if the file chunked to nothing (e.g. an empty
// file) — there is nothing to ingest, not a failure.
func reingestSource(ctx context.Context, store *falkorstore.Store, embedder *embedding.Client, f string, strict bool) (*SourceIngestReport, error) {
	chs, err := chunking.ChunkFile(f)
	if err != nil {
		return nil, fmt.Errorf("chunking %s: %w", f, err)
	}
	if len(chs) == 0 {
		return nil, nil
	}
	source := chs[0].Source

	fileID, err := store.AddFile(ctx, source, filepath.Base(source), filepath.Ext(source))
	if err != nil {
		return nil, fmt.Errorf("adding file node %s: %w", f, err)
	}

	existing, err := store.FetchChunkAnchors(ctx, source)
	if err != nil {
		return nil, fmt.Errorf("fetching existing chunk state for %s: %w", f, err)
	}
	existingByAnchor := make(map[string]falkorstore.ChunkAnchorRow, len(existing))
	for _, row := range existing {
		if row.AnchorID != "" {
			existingByAnchor[row.AnchorID] = row
		}
	}

	// Compute identity for every new chunk up front, and decide which ones
	// actually need embedding — skip anything whose anchor_id AND
	// content_hash both already match existing state (predicted
	// ChunkUnchanged) and anything oversized. This is what lets a re-ingest
	// of an unchanged file cost zero embed calls, not just zero writes.
	type prepared struct {
		chunk       chunking.Chunk
		anchorID    string
		contentHash string
		oversized   bool
		needsEmbed  bool
	}
	items := make([]prepared, len(chs))
	var embedTexts []string
	var embedIndex []int
	for i, c := range chs {
		anchorID := chunking.ComputeAnchorID(source, c.Breadcrumb)
		contentHash := chunking.ComputeContentHash(c.Content)
		oversized := len(c.Content) > OversizedChunkChars
		predictedUnchanged := false
		if row, ok := existingByAnchor[anchorID]; ok && row.ContentHash == contentHash {
			predictedUnchanged = true
		}
		needsEmbed := !oversized && !predictedUnchanged
		items[i] = prepared{chunk: c, anchorID: anchorID, contentHash: contentHash, oversized: oversized, needsEmbed: needsEmbed}
		if needsEmbed {
			embedTexts = append(embedTexts, c.Breadcrumb+"\n"+c.Content)
			embedIndex = append(embedIndex, i)
		}
	}

	var vectors [][]float32
	if len(embedTexts) > 0 {
		vectors, err = embedder.Embed(ctx, embedTexts)
		if err != nil {
			return nil, fmt.Errorf("embedding %s: %w", f, err)
		}
	}
	vectorFor := make([][]float32, len(items))
	for i, idx := range embedIndex {
		vectorFor[idx] = vectors[i]
	}

	report := &SourceIngestReport{Source: source}
	claimedIDs := make(map[int64]bool, len(existing))
	var keepIDs []int64
	for i, it := range items {
		id, action, err := store.UpsertChunk(ctx, source, it.chunk, vectorFor[i], it.anchorID, it.contentHash, it.oversized, fileID, existing, claimedIDs)
		if err != nil {
			return nil, fmt.Errorf("resolving chunk %d of %s: %w", i, f, err)
		}
		keepIDs = append(keepIDs, id)
		switch action {
		case falkorstore.ChunkCreated:
			report.Created = append(report.Created, id)
		case falkorstore.ChunkUpdated:
			report.Updated = append(report.Updated, id)
		case falkorstore.ChunkRenamed:
			report.Renamed = append(report.Renamed, id)
		case falkorstore.ChunkUnchanged:
			report.Unchanged = append(report.Unchanged, id)
		}
		if it.oversized {
			report.OversizedChunks++
			if action != falkorstore.ChunkUnchanged {
				log.Printf("oversized chunk skipped from embedding: %s lines %d-%d (%d chars) %q", source, it.chunk.LineStart, it.chunk.LineEnd, len(it.chunk.Content), it.chunk.Breadcrumb)
			}
		}
	}

	orphaned, err := store.MarkOrphanedChunks(ctx, source, keepIDs, time.Now().UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("marking orphaned chunks for %s: %w", f, err)
	}
	report.Orphaned = orphaned

	edges, err := doclinker.New(store).LinkBySource(ctx, source, strict)
	if err != nil {
		return nil, fmt.Errorf("linking %s: %w", f, err)
	}
	report.EdgesLinked = edges

	return report, nil
}

// AnalyzeRepository runs the full per-repo pipeline: tree-sitter + LSP code
// analysis, doc ingestion (honoring `ignore`), and a final full re-link so
// existing chunks pick up newly-discovered code entities. The store and
// embedder must already be configured for the target repo's graph; the caller
// is responsible for opening and closing them.
func AnalyzeRepository(ctx context.Context, store *falkorstore.Store, embedder *embedding.Client, lspCmd, path string, ignore []string) (AnalyzeResult, error) {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return AnalyzeResult{}, fmt.Errorf("path %q is not a valid directory", path)
	}
	mergedIgnore := analyzer.MergeIgnore(ignore)

	tsAnalyzer := analyzer.NewTypeScriptAnalyzer()
	defer tsAnalyzer.Close()

	orch := analyzer.NewOrchestrator(tsAnalyzer, store, lspCmd, []string{"--stdio"})
	codeResult, err := orch.Analyze(ctx, path, mergedIgnore)
	if err != nil {
		return AnalyzeResult{}, fmt.Errorf("analysis failed: %w", err)
	}

	if err := store.EnsureIndex(ctx, EmbeddingDim); err != nil {
		return AnalyzeResult{}, fmt.Errorf("ensuring chunk vector index: %w", err)
	}

	docs, err := IngestDocs(ctx, store, embedder, []string{path}, mergedIgnore, false)
	if err != nil {
		return AnalyzeResult{}, fmt.Errorf("ingesting docs: %w", err)
	}

	docEdges, err := doclinker.New(store).LinkRepo(ctx, false)
	if err != nil {
		return AnalyzeResult{}, fmt.Errorf("linking docs: %w", err)
	}

	return AnalyzeResult{
		Files:           codeResult.Files,
		Entities:        codeResult.Entities,
		Relationships:   codeResult.Relationships,
		AnalyzeErrors:   codeResult.Errors,
		DocChunks:       docs.ChunksStored,
		OversizedChunks: docs.OversizedChunks,
		DocEdges:        docEdges,
	}, nil
}

// LinkRepo runs the doc-to-code linker over every chunk in the store. Idempotent.
func LinkRepo(ctx context.Context, store *falkorstore.Store, strict bool) (int, error) {
	return doclinker.New(store).LinkRepo(ctx, strict)
}
