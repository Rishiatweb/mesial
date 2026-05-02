package main

import (
	"context"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/mknw/h9s/internal/analyzer"
	"github.com/mknw/h9s/internal/chunking"
	"github.com/mknw/h9s/internal/doclinker"
	"github.com/mknw/h9s/internal/embedding"
	"github.com/mknw/h9s/internal/falkorstore"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	defaultFalkorAddr   = "localhost:6381"
	defaultEmbeddingURL = "http://localhost:8090"
	defaultGraph        = "h9s"
	defaultDim          = 512 // Matryoshka truncation from native 1024
)

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// resolveRepoGraphName picks the FalkorDB graph name for a repo-scoped operation.
// Explicit name wins; otherwise walk up from anyPath looking for a .git directory
// and use filepath.Base of the directory containing it. Errors if neither resolves.
func resolveRepoGraphName(explicitRepo, anyPath string) (string, error) {
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

// oversizedChunkChars is the per-chunk char ceiling above which we skip
// embedding (would dilute the vector and risks blowing the embedding model's
// context). Oversized chunks are still stored with full content, OF_FILE, and
// oversized=true so the linker scans them and graph traversal sees them.
const oversizedChunkChars = 6000

// ingestDocsToStore expands the given paths to .md files, then per source:
// MERGEs a :File node, deletes existing chunks for that source, chunks +
// embeds + stores new chunks anchored to the file via :OF_FILE, and runs the
// doc linker (LinkBySource) to assert DOCUMENTS edges to code entities.
// Chunks longer than oversizedChunkChars are stored without a vector.
// Returns (chunks stored, oversized chunks, edges asserted).
func ingestDocsToStore(ctx context.Context, store *falkorstore.Store, embedder *embedding.Client, paths, ignore []string, strict bool) (int, int, int, error) {
	ignoreSet := make(map[string]bool, len(ignore))
	for _, name := range ignore {
		ignoreSet[name] = true
	}

	var files []string
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return 0, 0, 0, fmt.Errorf("stat %s: %w", p, err)
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
				return 0, 0, 0, fmt.Errorf("scanning %s: %w", p, err)
			}
		} else if strings.HasSuffix(strings.ToLower(p), ".md") {
			files = append(files, p)
		}
	}

	linker := doclinker.New(store)
	totalStored, totalOversized, totalEdges := 0, 0, 0
	for _, f := range files {
		chs, err := chunking.ChunkFile(f)
		if err != nil {
			return totalStored, totalOversized, totalEdges, fmt.Errorf("chunking %s: %w", f, err)
		}
		if len(chs) == 0 {
			continue
		}
		source := chs[0].Source

		fileID, err := store.AddFile(ctx, source, filepath.Base(source), filepath.Ext(source))
		if err != nil {
			return totalStored, totalOversized, totalEdges, fmt.Errorf("adding file node %s: %w", f, err)
		}

		if _, err := store.DeleteBySource(ctx, source); err != nil {
			return totalStored, totalOversized, totalEdges, fmt.Errorf("deleting old chunks for %s: %w", f, err)
		}

		// Partition into normal (will be embedded) and oversized (no vector).
		var normal, oversized []chunking.Chunk
		for _, c := range chs {
			if len(c.Content) > oversizedChunkChars {
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
				return totalStored, totalOversized, totalEdges, fmt.Errorf("embedding %s: %w", f, err)
			}
			fileIDs := make([]int64, len(normal))
			for i := range fileIDs {
				fileIDs[i] = fileID
			}
			n, err := store.StoreChunks(ctx, normal, vectors, fileIDs)
			if err != nil {
				return totalStored, totalOversized, totalEdges, fmt.Errorf("storing chunks for %s: %w", f, err)
			}
			totalStored += n
		}

		if len(oversized) > 0 {
			fileIDs := make([]int64, len(oversized))
			for i := range fileIDs {
				fileIDs[i] = fileID
			}
			n, err := store.StoreOversizedChunks(ctx, oversized, fileIDs)
			if err != nil {
				return totalStored, totalOversized, totalEdges, fmt.Errorf("storing oversized chunks for %s: %w", f, err)
			}
			totalOversized += n
			for _, c := range oversized {
				log.Printf("oversized chunk skipped from embedding: %s lines %d-%d (%d chars) %q", source, c.LineStart, c.LineEnd, len(c.Content), c.Breadcrumb)
			}
		}

		edges, err := linker.LinkBySource(ctx, source, strict)
		if err != nil {
			return totalStored, totalOversized, totalEdges, fmt.Errorf("linking %s: %w", f, err)
		}
		totalEdges += edges
	}

	return totalStored, totalOversized, totalEdges, nil
}

func main() {
	falkorAddr := env("FALKOR_ADDR", defaultFalkorAddr)
	embeddingURL := env("EMBEDDING_URL", defaultEmbeddingURL)
	graphName := env("H9S_GRAPH", defaultGraph)
	transport := env("H9S_TRANSPORT", "stdio")
	port := env("H9S_PORT", "8091")

	store, err := falkorstore.NewStore(falkorAddr, graphName)
	if err != nil {
		log.Fatalf("connecting to FalkorDB: %v", err)
	}
	defer store.Close()

	embedder := embedding.NewClient(embeddingURL, defaultDim)

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "h9s-ingest",
		Version: "0.1.0",
	}, nil)

	// --- ingest_documents tool ---

	type IngestInput struct {
		Paths  []string `json:"paths"            jsonschema:"File or directory paths to ingest. Accepts .md files or directories (scanned recursively)."`
		Repo   string   `json:"repo,omitempty"   jsonschema:"Optional repo name (FalkorDB graph). If omitted, resolved from the nearest .git ancestor of Paths[0]; all other paths must share that ancestor."`
		Strict bool     `json:"strict,omitempty" jsonschema:"If true, only backtick-fenced identifiers are eligible for DOCUMENTS edges (linker pass)."`
	}

	mcp.AddTool(server, &mcp.Tool{
		Name:        "ingest_documents",
		Description: "Chunk markdown files, embed them, and store vectors in a per-repo FalkorDB graph alongside the code graph. Each chunk is linked to its source file via OF_FILE.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input IngestInput) (*mcp.CallToolResult, any, error) {
		if len(input.Paths) == 0 {
			return nil, nil, fmt.Errorf("paths is required")
		}

		// Resolve repo from explicit name or .git walk-up of Paths[0].
		repoName, err := resolveRepoGraphName(input.Repo, input.Paths[0])
		if err != nil {
			return nil, nil, fmt.Errorf("resolving repo: %w", err)
		}
		// All other paths must resolve to the same repo when no explicit Repo given.
		if input.Repo == "" {
			for _, p := range input.Paths[1:] {
				other, err := resolveRepoGraphName("", p)
				if err != nil {
					return nil, nil, fmt.Errorf("resolving repo for %s: %w", p, err)
				}
				if other != repoName {
					return nil, nil, fmt.Errorf("paths span multiple repos: %q and %q", repoName, other)
				}
			}
		}

		repoStore, err := falkorstore.NewStore(falkorAddr, repoName)
		if err != nil {
			return nil, nil, fmt.Errorf("connecting to FalkorDB for graph %q: %w", repoName, err)
		}
		defer repoStore.Close()

		if err := repoStore.EnsureIndex(ctx, defaultDim); err != nil {
			return nil, nil, fmt.Errorf("ensuring chunk vector index: %w", err)
		}
		if err := repoStore.EnsureCodeIndex(ctx); err != nil {
			return nil, nil, fmt.Errorf("ensuring code index: %w", err)
		}

		stored, oversized, edges, err := ingestDocsToStore(ctx, repoStore, embedder, input.Paths, nil, input.Strict)
		if err != nil {
			return nil, nil, err
		}

		if stored == 0 && oversized == 0 {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("No .md files found in the provided paths (graph %q).", repoName)}},
			}, nil, nil
		}

		summary := fmt.Sprintf("Ingested %d chunks (%d oversized, no vector) into graph %q; %d DOCUMENTS edges asserted.", stored, oversized, repoName, edges)
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: summary}},
		}, nil, nil
	})

	// --- search_documents tool ---

	type SearchInput struct {
		Query string `json:"query"          jsonschema:"Natural language query to search for in the document store."`
		K     int    `json:"k"              jsonschema:"Number of results to return. Defaults to 5 if omitted."`
		Repo  string `json:"repo,omitempty" jsonschema:"Optional repo name (FalkorDB graph). If omitted, resolved from .git ancestor of Path; falls back to the global graph if neither is given."`
		Path  string `json:"path,omitempty" jsonschema:"Optional path inside a repo. Used for .git walk-up if Repo is not given."`
	}

	mcp.AddTool(server, &mcp.Tool{
		Name:        "search_documents",
		Description: "Semantic search over ingested documents. Embeds the query and returns the top-k most similar chunks. Targets a per-repo graph when Repo or Path is set; otherwise searches the global graph.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input SearchInput) (*mcp.CallToolResult, any, error) {
		k := input.K
		if k <= 0 {
			k = 5
		}

		// Pick target store: explicit Repo > .git walk-up from Path > global fallback.
		searchStore := store
		searchGraph := graphName
		if input.Repo != "" || input.Path != "" {
			repoName, err := resolveRepoGraphName(input.Repo, input.Path)
			if err != nil {
				return nil, nil, fmt.Errorf("resolving repo: %w", err)
			}
			repoStore, err := falkorstore.NewStore(falkorAddr, repoName)
			if err != nil {
				return nil, nil, fmt.Errorf("connecting to FalkorDB for graph %q: %w", repoName, err)
			}
			defer repoStore.Close()
			searchStore = repoStore
			searchGraph = repoName
		}

		// Embed the query
		vectors, err := embedder.Embed(ctx, []string{input.Query})
		if err != nil {
			return nil, nil, fmt.Errorf("embedding query: %w", err)
		}

		// Search
		results, err := searchStore.Search(ctx, vectors[0], k)
		if err != nil {
			return nil, nil, fmt.Errorf("searching graph %q: %w", searchGraph, err)
		}

		if len(results) == 0 {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "No results found."}},
			}, nil, nil
		}

		// Format results
		var sb strings.Builder
		for i, r := range results {
			fmt.Fprintf(&sb, "### Result %d (score: %.4f)\n", i+1, r.Score)
			fmt.Fprintf(&sb, "**Source:** %s (lines %d–%d)\n", r.Source, r.LineStart, r.LineEnd)
			fmt.Fprintf(&sb, "**Breadcrumb:** %s\n\n", r.Breadcrumb)
			content := r.Content
			if len(content) > 500 {
				content = content[:500] + "..."
			}
			fmt.Fprintf(&sb, "%s\n\n---\n\n", content)
		}

		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: sb.String()}},
		}, nil, nil
	})

	// --- analyze_repository tool ---

	lspCmd := env("H9S_LSP_CMD", "typescript-language-server")

	type AnalyzeInput struct {
		Path   string   `json:"path"   jsonschema:"Absolute filesystem path to the repository root."`
		Ignore []string `json:"ignore" jsonschema:"Directory names to skip (e.g. node_modules, dist, .git). Uses sensible defaults if omitted."`
	}

	mcp.AddTool(server, &mcp.Tool{
		Name:        "analyze_repository",
		Description: "Analyze a TypeScript/TSX codebase: parse source with tree-sitter, resolve symbols via LSP, and store the code graph in FalkorDB. Creates one graph per repository.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input AnalyzeInput) (*mcp.CallToolResult, any, error) {
		if input.Path == "" {
			return nil, nil, fmt.Errorf("path is required")
		}

		info, err := os.Stat(input.Path)
		if err != nil || !info.IsDir() {
			return nil, nil, fmt.Errorf("path %q is not a valid directory", input.Path)
		}

		repoGraphName := filepath.Base(input.Path)
		ignore := analyzer.MergeIgnore(input.Ignore)

		// Create a separate store for this repo's graph
		repoStore, err := falkorstore.NewStore(falkorAddr, repoGraphName)
		if err != nil {
			return nil, nil, fmt.Errorf("connecting to FalkorDB for graph %q: %w", repoGraphName, err)
		}
		defer repoStore.Close()

		tsAnalyzer := analyzer.NewTypeScriptAnalyzer()
		defer tsAnalyzer.Close()

		orch := analyzer.NewOrchestrator(tsAnalyzer, repoStore, lspCmd, []string{"--stdio"})
		result, err := orch.Analyze(ctx, input.Path, ignore)
		if err != nil {
			return nil, nil, fmt.Errorf("analysis failed: %w", err)
		}

		// Ensure chunk vector index exists in this repo's graph before doc ingest.
		if err := repoStore.EnsureIndex(ctx, defaultDim); err != nil {
			return nil, nil, fmt.Errorf("ensuring chunk vector index: %w", err)
		}

		// Pass 3: ingest .md files in the repo, honoring the same ignore set
		// the analyzer used so we don't suck in node_modules/**/*.md.
		docChunks, oversized, _, err := ingestDocsToStore(ctx, repoStore, embedder, []string{input.Path}, ignore, false)
		if err != nil {
			return nil, nil, fmt.Errorf("ingesting docs: %w", err)
		}

		// Final full re-link so chunks ingested earlier (or before this code
		// existed in the repo's graph) pick up new code entities.
		linker := doclinker.New(repoStore)
		docEdges, err := linker.LinkRepo(ctx, false)
		if err != nil {
			return nil, nil, fmt.Errorf("linking docs: %w", err)
		}

		summary := fmt.Sprintf(
			"Analyzed %q → graph %q: %d files, %d entities, %d relationships (%d errors); %d doc chunks (%d oversized), %d DOCUMENTS edges.",
			input.Path, repoGraphName, result.Files, result.Entities, result.Relationships, result.Errors, docChunks, oversized, docEdges,
		)
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: summary}},
		}, nil, nil
	})

	// --- link_docs tool ---

	type LinkDocsInput struct {
		Repo   string `json:"repo,omitempty"   jsonschema:"Repo name (FalkorDB graph). If omitted, resolved from .git ancestor of Path."`
		Path   string `json:"path,omitempty"   jsonschema:"Path inside the target repo, used for .git walk-up if Repo is not given."`
		Strict bool   `json:"strict,omitempty" jsonschema:"If true, only backtick-fenced identifiers create DOCUMENTS edges."`
	}

	mcp.AddTool(server, &mcp.Tool{
		Name:        "link_docs",
		Description: "Re-run the doc-to-code linker over an existing per-repo graph. Idempotent: emits (:Chunk)-[:DOCUMENTS]->(code-entity) edges where chunk content mentions a known entity name.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input LinkDocsInput) (*mcp.CallToolResult, any, error) {
		repoName, err := resolveRepoGraphName(input.Repo, input.Path)
		if err != nil {
			return nil, nil, fmt.Errorf("resolving repo: %w", err)
		}
		repoStore, err := falkorstore.NewStore(falkorAddr, repoName)
		if err != nil {
			return nil, nil, fmt.Errorf("connecting to FalkorDB for graph %q: %w", repoName, err)
		}
		defer repoStore.Close()

		edges, err := doclinker.New(repoStore).LinkRepo(ctx, input.Strict)
		if err != nil {
			return nil, nil, fmt.Errorf("linking: %w", err)
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Asserted %d DOCUMENTS edges in graph %q.", edges, repoName)}},
		}, nil, nil
	})

	// --- run server ---

	ctx := context.Background()

	switch transport {
	case "http":
		handler := mcp.NewStreamableHTTPHandler(
			func(r *http.Request) *mcp.Server { return server },
			nil,
		)
		log.Printf("h9s-ingest: HTTP mode on :%s (falkor=%s, embedding=%s)", port, falkorAddr, embeddingURL)
		log.Fatal(http.ListenAndServe(":"+port, handler))
	default:
		log.Printf("h9s-ingest: stdio mode (falkor=%s, embedding=%s)", falkorAddr, embeddingURL)
		if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil {
			log.Fatal(err)
		}
	}
}
