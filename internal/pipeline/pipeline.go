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
type IngestResult struct {
	ChunksStored    int
	OversizedChunks int
	EdgesAsserted   int
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

	linker := doclinker.New(store)
	var res IngestResult
	for _, f := range files {
		chs, err := chunking.ChunkFile(f)
		if err != nil {
			return res, fmt.Errorf("chunking %s: %w", f, err)
		}
		if len(chs) == 0 {
			continue
		}
		source := chs[0].Source

		fileID, err := store.AddFile(ctx, source, filepath.Base(source), filepath.Ext(source))
		if err != nil {
			return res, fmt.Errorf("adding file node %s: %w", f, err)
		}

		if _, err := store.DeleteBySource(ctx, source); err != nil {
			return res, fmt.Errorf("deleting old chunks for %s: %w", f, err)
		}

		var normal, oversized []chunking.Chunk
		for _, c := range chs {
			if len(c.Content) > OversizedChunkChars {
				oversized = append(oversized, c)
			} else {
				normal = append(normal, c)
			}
		}

		if len(normal) > 0 {
			texts := make([]string, len(normal))
			for i, c := range normal {
				texts[i] = c.Breadcrumb + "\n" + c.Content
			}
			vectors, err := embedder.Embed(ctx, texts)
			if err != nil {
				return res, fmt.Errorf("embedding %s: %w", f, err)
			}
			fileIDs := make([]int64, len(normal))
			for i := range fileIDs {
				fileIDs[i] = fileID
			}
			n, err := store.StoreChunks(ctx, normal, vectors, fileIDs)
			if err != nil {
				return res, fmt.Errorf("storing chunks for %s: %w", f, err)
			}
			res.ChunksStored += n
		}

		if len(oversized) > 0 {
			fileIDs := make([]int64, len(oversized))
			for i := range fileIDs {
				fileIDs[i] = fileID
			}
			n, err := store.StoreOversizedChunks(ctx, oversized, fileIDs)
			if err != nil {
				return res, fmt.Errorf("storing oversized chunks for %s: %w", f, err)
			}
			res.OversizedChunks += n
			for _, c := range oversized {
				log.Printf("oversized chunk skipped from embedding: %s lines %d-%d (%d chars) %q", source, c.LineStart, c.LineEnd, len(c.Content), c.Breadcrumb)
			}
		}

		edges, err := linker.LinkBySource(ctx, source, strict)
		if err != nil {
			return res, fmt.Errorf("linking %s: %w", f, err)
		}
		res.EdgesAsserted += edges
	}

	return res, nil
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
