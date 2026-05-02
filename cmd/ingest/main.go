// Command ingest is the MCP server entrypoint. Each tool handler is a thin
// adapter over functions in internal/pipeline; the same pipeline is used by
// the dev CLI in cmd/h9s-cli, so MCP and CLI behavior stay in lock-step.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/mknw/h9s/internal/embedding"
	"github.com/mknw/h9s/internal/falkorstore"
	"github.com/mknw/h9s/internal/pipeline"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	defaultFalkorAddr   = "localhost:6381"
	defaultEmbeddingURL = "http://localhost:8090"
	defaultGraph        = "h9s"
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
	lspCmd := env("H9S_LSP_CMD", "typescript-language-server")

	store, err := falkorstore.NewStore(falkorAddr, graphName)
	if err != nil {
		log.Fatalf("connecting to FalkorDB: %v", err)
	}
	defer store.Close()

	embedder := embedding.NewClient(embeddingURL, pipeline.EmbeddingDim)

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "h9s-ingest",
		Version: "0.1.0",
	}, nil)

	// --- ingest_documents ---

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

		repoName, err := pipeline.ResolvePathsToRepo(input.Repo, input.Paths)
		if err != nil {
			return nil, nil, fmt.Errorf("resolving repo: %w", err)
		}

		repoStore, err := falkorstore.NewStore(falkorAddr, repoName)
		if err != nil {
			return nil, nil, fmt.Errorf("connecting to FalkorDB for graph %q: %w", repoName, err)
		}
		defer repoStore.Close()

		if err := repoStore.EnsureIndex(ctx, pipeline.EmbeddingDim); err != nil {
			return nil, nil, fmt.Errorf("ensuring chunk vector index: %w", err)
		}
		if err := repoStore.EnsureCodeIndex(ctx); err != nil {
			return nil, nil, fmt.Errorf("ensuring code index: %w", err)
		}

		res, err := pipeline.IngestDocs(ctx, repoStore, embedder, input.Paths, nil, input.Strict)
		if err != nil {
			return nil, nil, err
		}

		if res.ChunksStored == 0 && res.OversizedChunks == 0 {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("No .md files found in the provided paths (graph %q).", repoName)}},
			}, nil, nil
		}

		summary := fmt.Sprintf("Ingested %d chunks (%d oversized, no vector) into graph %q; %d DOCUMENTS edges asserted.",
			res.ChunksStored, res.OversizedChunks, repoName, res.EdgesAsserted)
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: summary}},
		}, nil, nil
	})

	// --- search_documents ---

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

		searchStore := store
		searchGraph := graphName
		if input.Repo != "" || input.Path != "" {
			repoName, err := pipeline.ResolveRepoGraphName(input.Repo, input.Path)
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

		vectors, err := embedder.Embed(ctx, []string{input.Query})
		if err != nil {
			return nil, nil, fmt.Errorf("embedding query: %w", err)
		}
		results, err := searchStore.Search(ctx, vectors[0], k)
		if err != nil {
			return nil, nil, fmt.Errorf("searching graph %q: %w", searchGraph, err)
		}

		if len(results) == 0 {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "No results found."}},
			}, nil, nil
		}

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

	// --- analyze_repository ---

	type AnalyzeInput struct {
		Path   string   `json:"path"            jsonschema:"Absolute filesystem path to the repository root."`
		Ignore []string `json:"ignore"          jsonschema:"Directory names to skip (e.g. node_modules, dist, .git). Uses sensible defaults if omitted."`
		Repo   string   `json:"repo,omitempty"  jsonschema:"Optional repo name (FalkorDB graph) to override the default. If omitted, uses the .git ancestor of Path, falling back to filepath.Base(Path)."`
	}

	mcp.AddTool(server, &mcp.Tool{
		Name:        "analyze_repository",
		Description: "Analyze a TypeScript/TSX codebase: parse source with tree-sitter, resolve symbols via LSP, ingest in-repo markdown, and emit DOCUMENTS edges. Creates one graph per repository.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input AnalyzeInput) (*mcp.CallToolResult, any, error) {
		if input.Path == "" {
			return nil, nil, fmt.Errorf("path is required")
		}

		repoName := input.Repo
		if repoName == "" {
			if name, err := pipeline.ResolveRepoGraphName("", input.Path); err == nil {
				repoName = name
			} else {
				// Not a git repo (e.g. an exported sample) — fall back to basename.
				repoName = filepath.Base(input.Path)
			}
		}

		repoStore, err := falkorstore.NewStore(falkorAddr, repoName)
		if err != nil {
			return nil, nil, fmt.Errorf("connecting to FalkorDB for graph %q: %w", repoName, err)
		}
		defer repoStore.Close()

		res, err := pipeline.AnalyzeRepository(ctx, repoStore, embedder, lspCmd, input.Path, input.Ignore)
		if err != nil {
			return nil, nil, err
		}

		summary := fmt.Sprintf(
			"Analyzed %q → graph %q: %d files, %d entities, %d relationships (%d errors); %d doc chunks (%d oversized), %d DOCUMENTS edges.",
			input.Path, repoName, res.Files, res.Entities, res.Relationships, res.AnalyzeErrors,
			res.DocChunks, res.OversizedChunks, res.DocEdges,
		)
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: summary}},
		}, nil, nil
	})

	// --- link_docs ---

	type LinkDocsInput struct {
		Repo   string `json:"repo,omitempty"   jsonschema:"Repo name (FalkorDB graph). If omitted, resolved from .git ancestor of Path."`
		Path   string `json:"path,omitempty"   jsonschema:"Path inside the target repo, used for .git walk-up if Repo is not given."`
		Strict bool   `json:"strict,omitempty" jsonschema:"If true, only backtick-fenced identifiers create DOCUMENTS edges."`
	}

	mcp.AddTool(server, &mcp.Tool{
		Name:        "link_docs",
		Description: "Re-run the doc-to-code linker over an existing per-repo graph. Idempotent.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input LinkDocsInput) (*mcp.CallToolResult, any, error) {
		repoName, err := pipeline.ResolveRepoGraphName(input.Repo, input.Path)
		if err != nil {
			return nil, nil, fmt.Errorf("resolving repo: %w", err)
		}
		repoStore, err := falkorstore.NewStore(falkorAddr, repoName)
		if err != nil {
			return nil, nil, fmt.Errorf("connecting to FalkorDB for graph %q: %w", repoName, err)
		}
		defer repoStore.Close()

		edges, err := pipeline.LinkRepo(ctx, repoStore, input.Strict)
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

