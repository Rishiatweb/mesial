// Command ingest is the MCP server entrypoint. Each tool handler is a thin
// adapter over functions in internal/pipeline; the same pipeline is used by
// the dev CLI in cmd/h9s-cli, so MCP and CLI behavior stay in lock-step.
package main

import (
	"context"
	"encoding/json"
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

	// --- add_observation ---
	//
	// Deep by design: embeds the text, KNN-searches existing observations,
	// creates the node, and optionally links MOTIVATES (to a chunk) and/or
	// EVIDENCE_FOR (to existing facts) — all in one call. Folds what would
	// otherwise be mandatory follow-up calls (link_motivates, link_evidence)
	// into the write path, since an agent recording an observation almost
	// always already knows the chunk it was reading, if any. link_evidence
	// stays as its own tool below for the genuinely different case of
	// retroactively linking an older observation to a newly confirmed fact.

	type AddObservationInput struct {
		Text               string  `json:"text"                            jsonschema:"Observation text — a sentence, rarely two, describing what was learned."`
		Repo               string  `json:"repo,omitempty"                  jsonschema:"Optional repo name (FalkorDB graph). If omitted, resolved from .git ancestor of Path; falls back to the global memory graph (H9S_GRAPH) if neither is given."`
		Path               string  `json:"path,omitempty"                  jsonschema:"Optional path inside a repo. Used for .git walk-up if Repo is not given."`
		K                  int     `json:"k,omitempty"                     jsonschema:"Number of nearest existing observations to consult for similar_facts. Defaults to 5."`
		MotivatesChunkID   int64   `json:"motivates_chunk_id,omitempty"    jsonschema:"Optional chunk ID this observation motivates. Links (:Observation)-[:MOTIVATES]->(:Chunk) in the same call."`
		EvidenceForFactIDs []int64 `json:"evidence_for_fact_ids,omitempty" jsonschema:"Optional existing fact IDs this observation backs. Links (:Observation)-[:EVIDENCE_FOR]->(:Fact) for each, in the same call."`
	}

	type AddObservationOutput struct {
		ObservationID      int64                  `json:"observation_id"`
		SimilarFacts       []pipeline.SimilarFact `json:"similar_facts"`
		MotivatesLinked    bool                   `json:"motivates_linked"`
		EvidenceLinkedIDs  []int64                `json:"evidence_linked_ids"`
	}

	mcp.AddTool(server, &mcp.Tool{
		Name:        "add_observation",
		Description: "Record an episodic observation in the memory layer. Embeds the text, KNN-searches existing observations, and returns similar facts the agent can decide to link. Optionally links MOTIVATES and/or EVIDENCE_FOR in the same call.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input AddObservationInput) (*mcp.CallToolResult, any, error) {
		if strings.TrimSpace(input.Text) == "" {
			return nil, nil, fmt.Errorf("text is required")
		}

		memStore := store
		memGraph := graphName
		if input.Repo != "" || input.Path != "" {
			name, err := pipeline.ResolveRepoGraphName(input.Repo, input.Path)
			if err != nil {
				return nil, nil, fmt.Errorf("resolving repo: %w", err)
			}
			repoStore, err := falkorstore.NewStore(falkorAddr, name)
			if err != nil {
				return nil, nil, fmt.Errorf("connecting to FalkorDB for graph %q: %w", name, err)
			}
			defer repoStore.Close()
			memStore = repoStore
			memGraph = name
		}

		if err := pipeline.EnsureMemoryReady(ctx, memStore, pipeline.MemoryGraphKind); err != nil {
			return nil, nil, fmt.Errorf("ensuring memory layer ready on graph %q: %w", memGraph, err)
		}

		res, err := pipeline.AddObservation(ctx, memStore, embedder, input.Text, input.K)
		if err != nil {
			return nil, nil, err
		}

		out := AddObservationOutput{
			ObservationID: res.ObservationID,
			SimilarFacts:  res.SimilarFacts,
		}

		if input.MotivatesChunkID > 0 {
			if err := pipeline.LinkObservationMotivatesChunk(ctx, memStore, res.ObservationID, input.MotivatesChunkID); err != nil {
				return nil, nil, fmt.Errorf("observation %d created, but linking MOTIVATES to chunk %d failed: %w", res.ObservationID, input.MotivatesChunkID, err)
			}
			out.MotivatesLinked = true
		}

		if len(input.EvidenceForFactIDs) > 0 {
			if err := pipeline.LinkObservationEvidence(ctx, memStore, res.ObservationID, input.EvidenceForFactIDs); err != nil {
				return nil, nil, fmt.Errorf("observation %d created, but linking EVIDENCE_FOR failed: %w", res.ObservationID, err)
			}
			out.EvidenceLinkedIDs = input.EvidenceForFactIDs
		}

		body, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return nil, nil, fmt.Errorf("encoding result: %w", err)
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: string(body)}},
		}, nil, nil
	})

	// --- create_fact ---

	type CreateFactInput struct {
		Repo          string `json:"repo,omitempty"        jsonschema:"Optional repo name (FalkorDB graph). If omitted, resolved from .git ancestor of Path; falls back to the global memory graph if neither is given."`
		Path          string `json:"path,omitempty"        jsonschema:"Optional path inside a repo. Used for .git walk-up if Repo is not given."`
		ObservationID int64  `json:"observation_id"        jsonschema:"ID of the backing :Observation (required). Every fact must have at least one — this is the only way to create one."`
		Subject       string `json:"subject"                jsonschema:"Fact subject."`
		Predicate     string `json:"predicate"              jsonschema:"Fact predicate. Kernel (inference-actionable): is_a, subtype_of, part_of, equivalent_to, incompatible_with, causes, requires. Open-set predicates are accepted but inert to the future inference engine."`
		Object        string `json:"object"                 jsonschema:"Fact object."`
	}

	mcp.AddTool(server, &mcp.Tool{
		Name:        "create_fact",
		Description: "MERGE a :Fact triplet and link it to a backing :Observation via EVIDENCE_FOR, atomically. The only path to creating a fact — enforces the no-orphan-facts invariant.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input CreateFactInput) (*mcp.CallToolResult, any, error) {
		if input.ObservationID <= 0 {
			return nil, nil, fmt.Errorf("observation_id is required")
		}
		if input.Subject == "" || input.Predicate == "" || input.Object == "" {
			return nil, nil, fmt.Errorf("subject, predicate, and object are all required")
		}

		memStore := store
		memGraph := graphName
		if input.Repo != "" || input.Path != "" {
			name, err := pipeline.ResolveRepoGraphName(input.Repo, input.Path)
			if err != nil {
				return nil, nil, fmt.Errorf("resolving repo: %w", err)
			}
			repoStore, err := falkorstore.NewStore(falkorAddr, name)
			if err != nil {
				return nil, nil, fmt.Errorf("connecting to FalkorDB for graph %q: %w", name, err)
			}
			defer repoStore.Close()
			memStore = repoStore
			memGraph = name
		}

		if err := pipeline.EnsureMemoryReady(ctx, memStore, pipeline.MemoryGraphKind); err != nil {
			return nil, nil, fmt.Errorf("ensuring memory layer ready on graph %q: %w", memGraph, err)
		}

		factID, err := pipeline.CreateFactFromObservation(ctx, memStore, input.ObservationID, input.Subject, input.Predicate, input.Object)
		if err != nil {
			return nil, nil, err
		}

		summary := fmt.Sprintf("Fact %d: (%s, %s, %s) — backed by observation %d, graph %q.",
			factID, input.Subject, input.Predicate, input.Object, input.ObservationID, memGraph)
		if !falkorstore.MemoryPredicateKernel[input.Predicate] {
			summary += fmt.Sprintf(" Note: predicate %q is open-set — accepted but inert to the future inference engine.", input.Predicate)
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: summary}},
		}, nil, nil
	})

	// --- link_evidence ---
	//
	// Kept as its own tool: retroactively linking an older observation to a
	// newly confirmed fact is a genuinely different case from linking at
	// write time (which add_observation already covers) — a real second
	// adapter, not a convenience duplicate.

	type LinkEvidenceInput struct {
		Repo          string  `json:"repo,omitempty"        jsonschema:"Optional repo name (FalkorDB graph). If omitted, resolved from .git ancestor of Path; falls back to the global memory graph if neither is given."`
		Path          string  `json:"path,omitempty"        jsonschema:"Optional path inside a repo. Used for .git walk-up if Repo is not given."`
		ObservationID int64   `json:"observation_id"        jsonschema:"Observation ID (required)."`
		FactIDs       []int64 `json:"fact_ids"              jsonschema:"Existing fact IDs to link as evidence (required, non-empty)."`
	}

	mcp.AddTool(server, &mcp.Tool{
		Name:        "link_evidence",
		Description: "MERGE EVIDENCE_FOR edges from an existing observation to one or more existing facts. For retroactive linking; add_observation already links evidence supplied at write time.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input LinkEvidenceInput) (*mcp.CallToolResult, any, error) {
		if input.ObservationID <= 0 {
			return nil, nil, fmt.Errorf("observation_id is required")
		}
		if len(input.FactIDs) == 0 {
			return nil, nil, fmt.Errorf("fact_ids is required")
		}

		memStore := store
		memGraph := graphName
		if input.Repo != "" || input.Path != "" {
			name, err := pipeline.ResolveRepoGraphName(input.Repo, input.Path)
			if err != nil {
				return nil, nil, fmt.Errorf("resolving repo: %w", err)
			}
			repoStore, err := falkorstore.NewStore(falkorAddr, name)
			if err != nil {
				return nil, nil, fmt.Errorf("connecting to FalkorDB for graph %q: %w", name, err)
			}
			defer repoStore.Close()
			memStore = repoStore
			memGraph = name
		}

		if err := pipeline.LinkObservationEvidence(ctx, memStore, input.ObservationID, input.FactIDs); err != nil {
			return nil, nil, err
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Linked observation %d to %d fact(s) via EVIDENCE_FOR, graph %q.", input.ObservationID, len(input.FactIDs), memGraph)}},
		}, nil, nil
	})

	// --- search_observations ---

	type SearchObservationsInput struct {
		Query string `json:"query"          jsonschema:"Natural language query to search for in the observation store."`
		K     int    `json:"k,omitempty"    jsonschema:"Number of results to return. Defaults to 5 if omitted."`
		Repo  string `json:"repo,omitempty" jsonschema:"Optional repo name (FalkorDB graph). If omitted, resolved from .git ancestor of Path; falls back to the global memory graph if neither is given."`
		Path  string `json:"path,omitempty" jsonschema:"Optional path inside a repo. Used for .git walk-up if Repo is not given."`
	}

	mcp.AddTool(server, &mcp.Tool{
		Name:        "search_observations",
		Description: "KNN search over the memory layer's observations. Returns hits with cosine distance.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input SearchObservationsInput) (*mcp.CallToolResult, any, error) {
		memStore := store
		if input.Repo != "" || input.Path != "" {
			name, err := pipeline.ResolveRepoGraphName(input.Repo, input.Path)
			if err != nil {
				return nil, nil, fmt.Errorf("resolving repo: %w", err)
			}
			repoStore, err := falkorstore.NewStore(falkorAddr, name)
			if err != nil {
				return nil, nil, fmt.Errorf("connecting to FalkorDB for graph %q: %w", name, err)
			}
			defer repoStore.Close()
			memStore = repoStore
		}

		hits, err := pipeline.SearchObservations(ctx, memStore, embedder, input.Query, input.K)
		if err != nil {
			return nil, nil, err
		}
		body, err := json.MarshalIndent(hits, "", "  ")
		if err != nil {
			return nil, nil, fmt.Errorf("encoding result: %w", err)
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: string(body)}},
		}, nil, nil
	})

	// --- search_facts ---

	type SearchFactsInput struct {
		Subject   string `json:"subject,omitempty"   jsonschema:"Exact subject filter (empty = wildcard)."`
		Predicate string `json:"predicate,omitempty" jsonschema:"Exact predicate filter (empty = wildcard)."`
		Object    string `json:"object,omitempty"    jsonschema:"Exact object filter (empty = wildcard)."`
		Limit     int    `json:"limit,omitempty"     jsonschema:"Max rows. Defaults to 50."`
		Repo      string `json:"repo,omitempty"      jsonschema:"Optional repo name (FalkorDB graph). If omitted, resolved from .git ancestor of Path; falls back to the global memory graph if neither is given."`
		Path      string `json:"path,omitempty"      jsonschema:"Optional path inside a repo. Used for .git walk-up if Repo is not given."`
	}

	mcp.AddTool(server, &mcp.Tool{
		Name:        "search_facts",
		Description: "Structural search over the memory layer's facts (subject/predicate/object filters).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input SearchFactsInput) (*mcp.CallToolResult, any, error) {
		memStore := store
		if input.Repo != "" || input.Path != "" {
			name, err := pipeline.ResolveRepoGraphName(input.Repo, input.Path)
			if err != nil {
				return nil, nil, fmt.Errorf("resolving repo: %w", err)
			}
			repoStore, err := falkorstore.NewStore(falkorAddr, name)
			if err != nil {
				return nil, nil, fmt.Errorf("connecting to FalkorDB for graph %q: %w", name, err)
			}
			defer repoStore.Close()
			memStore = repoStore
		}

		rows, err := pipeline.SearchFacts(ctx, memStore, input.Subject, input.Predicate, input.Object, input.Limit)
		if err != nil {
			return nil, nil, err
		}
		body, err := json.MarshalIndent(rows, "", "  ")
		if err != nil {
			return nil, nil, fmt.Errorf("encoding result: %w", err)
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: string(body)}},
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

