package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/mknw/h9s/internal/analyzer"
	"github.com/mknw/h9s/internal/chunking"
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
		Paths []string `json:"paths" jsonschema:"File or directory paths to ingest. Accepts .md files or directories (scanned recursively)."`
	}

	mcp.AddTool(server, &mcp.Tool{
		Name:        "ingest_documents",
		Description: "Chunk markdown files, embed them, and store vectors in FalkorDB for semantic search.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input IngestInput) (*mcp.CallToolResult, any, error) {
		// Ensure vector index exists
		if err := store.EnsureIndex(ctx, defaultDim); err != nil {
			return nil, nil, fmt.Errorf("ensuring index: %w", err)
		}

		// Resolve files
		var files []string
		for _, p := range input.Paths {
			info, err := os.Stat(p)
			if err != nil {
				return nil, nil, fmt.Errorf("stat %s: %w", p, err)
			}
			if info.IsDir() {
				found, err := chunking.FindMarkdown(p)
				if err != nil {
					return nil, nil, fmt.Errorf("scanning %s: %w", p, err)
				}
				files = append(files, found...)
			} else {
				files = append(files, p)
			}
		}

		if len(files) == 0 {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "No .md files found in the provided paths."}},
			}, nil, nil
		}

		// Chunk all files
		var allChunks []chunking.Chunk
		for _, f := range files {
			chunks, err := chunking.ChunkFile(f)
			if err != nil {
				return nil, nil, fmt.Errorf("chunking %s: %w", f, err)
			}
			// Delete old nodes for this source before re-ingesting
			if _, err := store.DeleteBySource(ctx, chunks[0].Source); err != nil {
				return nil, nil, fmt.Errorf("deleting old nodes for %s: %w", f, err)
			}
			allChunks = append(allChunks, chunks...)
		}

		// Extract content texts for embedding
		texts := make([]string, len(allChunks))
		for i, c := range allChunks {
			texts[i] = c.Breadcrumb + "\n" + c.Content
		}

		// Embed
		vectors, err := embedder.Embed(ctx, texts)
		if err != nil {
			return nil, nil, fmt.Errorf("embedding: %w", err)
		}

		// Store
		stored, err := store.StoreChunks(ctx, allChunks, vectors)
		if err != nil {
			return nil, nil, fmt.Errorf("storing chunks: %w", err)
		}

		summary := fmt.Sprintf("Ingested %d chunks from %d files into graph %q.", stored, len(files), graphName)
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: summary}},
		}, nil, nil
	})

	// --- search_documents tool ---

	type SearchInput struct {
		Query string `json:"query" jsonschema:"Natural language query to search for in the document store."`
		K     int    `json:"k"     jsonschema:"Number of results to return. Defaults to 5 if omitted."`
	}

	mcp.AddTool(server, &mcp.Tool{
		Name:        "search_documents",
		Description: "Semantic search over ingested documents. Embeds the query and returns the top-k most similar chunks.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input SearchInput) (*mcp.CallToolResult, any, error) {
		k := input.K
		if k <= 0 {
			k = 5
		}

		// Embed the query
		vectors, err := embedder.Embed(ctx, []string{input.Query})
		if err != nil {
			return nil, nil, fmt.Errorf("embedding query: %w", err)
		}

		// Search
		results, err := store.Search(ctx, vectors[0], k)
		if err != nil {
			return nil, nil, fmt.Errorf("searching: %w", err)
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

		summary := fmt.Sprintf(
			"Analyzed %q → graph %q: %d files, %d entities, %d relationships (%d errors).",
			input.Path, repoGraphName, result.Files, result.Entities, result.Relationships, result.Errors,
		)
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: summary}},
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
