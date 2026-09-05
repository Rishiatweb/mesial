// Command h9s-cli is a developer harness over the same internal/pipeline
// package the MCP server uses. It bypasses MCP entirely — single process,
// direct calls into the pipeline, same arguments and same behavior as the
// MCP tools. Intended for fast iteration; not a replacement for the server.
//
// Subcommands:
//
//	h9s-cli analyze <path> [--ignore name1,...] [--repo NAME]
//	h9s-cli ingest  <path>... [--repo NAME] [--strict]
//	h9s-cli search  <query>   [--k N] [--repo NAME] [--path P]
//	h9s-cli link              [--repo NAME] [--path P] [--strict]
//	h9s-cli memory  <sub>     manage Fact/Observation memory layer (run "memory help" for details)
//
// Common flags (or env vars): --falkor-addr, --embedding-url, --lsp-cmd, --graph.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/mknw/h9s/internal/embedding"
	"github.com/mknw/h9s/internal/falkorstore"
	"github.com/mknw/h9s/internal/pipeline"
)

const (
	defaultFalkorAddr   = "localhost:6381"
	defaultEmbeddingURL = "http://localhost:8090"
	defaultGraph        = "h9s"
	defaultLSPCmd       = "typescript-language-server"
)

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]

	var err error
	switch cmd {
	case "analyze":
		err = runAnalyze(args)
	case "ingest":
		err = runIngest(args)
	case "search":
		err = runSearch(args)
	case "link":
		err = runLink(args)
	case "memory":
		err = runMemory(args)
	case "backfill-anchors":
		err = runBackfillAnchors(args)
	case "-h", "--help", "help":
		usage(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n\n", cmd)
		usage(os.Stderr)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage(w *os.File) {
	fmt.Fprint(w, `h9s-cli — direct dev harness for mesial

Usage:
  h9s-cli <subcommand> [args]

Subcommands:
  analyze <path>            Analyze a TypeScript repo: code graph + docs + linker
  ingest  <path>...         Chunk + embed + link markdown files (per-repo)
  search  <query>           Vector search over chunks
  link                      Re-run the doc-to-code linker on an existing graph
  memory  <sub>             Manage the Fact/Observation memory layer
                            (init, add-obs, create-fact, link-evidence,
                             link-motivates, search-obs, search-facts)
  backfill-anchors          One-time: compute anchor_id/content_hash on
                            existing :Chunk nodes that predate match-in-place
                            ingestion. Pure property SETs -- safe to re-run,
                            zero risk to node IDs or edges. Run this once per
                            graph before your next ingest_documents/
                            analyze_repository call, or the first re-ingest
                            will orphan every existing chunk once.

Common flags (override defaults / env):
  --falkor-addr   FalkorDB address       (env FALKOR_ADDR,   default `+defaultFalkorAddr+`)
  --embedding-url Embedding server URL   (env EMBEDDING_URL, default `+defaultEmbeddingURL+`)
  --lsp-cmd       LSP binary path        (env H9S_LSP_CMD,   default `+defaultLSPCmd+`)
  --graph         Default/global graph   (env H9S_GRAPH,     default `+defaultGraph+`)

Subcommand-specific flags:
  analyze: --repo NAME, --ignore name1,name2,...
  ingest:  --repo NAME, --strict
  search:  --k N, --repo NAME, --path PATH
  link:    --repo NAME, --path PATH, --strict

Run "h9s-cli <subcommand> --help" for per-subcommand details.
`)
}

// commonFlags wires the shared connection/config flags onto a FlagSet and
// returns pointers to the parsed values.
type commonFlags struct {
	falkorAddr   *string
	embeddingURL *string
	lspCmd       *string
	graph        *string
}

func bindCommon(fs *flag.FlagSet) *commonFlags {
	return &commonFlags{
		falkorAddr:   fs.String("falkor-addr", env("FALKOR_ADDR", defaultFalkorAddr), "FalkorDB address"),
		embeddingURL: fs.String("embedding-url", env("EMBEDDING_URL", defaultEmbeddingURL), "embedding server URL"),
		lspCmd:       fs.String("lsp-cmd", env("H9S_LSP_CMD", defaultLSPCmd), "LSP binary"),
		graph:        fs.String("graph", env("H9S_GRAPH", defaultGraph), "default/global FalkorDB graph"),
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// --- analyze ---

func runAnalyze(args []string) error {
	fs := flag.NewFlagSet("analyze", flag.ExitOnError)
	cf := bindCommon(fs)
	repo := fs.String("repo", "", "override graph name (default: .git ancestor or filepath.Base)")
	ignoreCSV := fs.String("ignore", "", "comma-separated dir names to skip (defaults to analyzer.MergeIgnore)")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "usage: h9s-cli analyze <path> [flags]\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		fs.Usage()
		return fmt.Errorf("path is required")
	}
	path := fs.Arg(0)

	repoName := *repo
	if repoName == "" {
		if name, err := pipeline.ResolveRepoGraphName("", path); err == nil {
			repoName = name
		} else {
			repoName = lastSegment(path)
		}
	}

	store, err := falkorstore.NewStore(*cf.falkorAddr, repoName)
	if err != nil {
		return fmt.Errorf("connecting to FalkorDB for graph %q: %w", repoName, err)
	}
	defer store.Close()
	embedder := embedding.NewClient(*cf.embeddingURL, pipeline.EmbeddingDim)

	ctx := context.Background()
	res, err := pipeline.AnalyzeRepository(ctx, store, embedder, *cf.lspCmd, path, splitCSV(*ignoreCSV))
	if err != nil {
		return err
	}
	fmt.Printf("Analyzed %q → graph %q: %d files, %d entities, %d relationships (%d errors); %d doc chunks (%d oversized), %d DOCUMENTS edges.\n",
		path, repoName, res.Files, res.Entities, res.Relationships, res.AnalyzeErrors,
		res.DocChunks, res.OversizedChunks, res.DocEdges)
	return nil
}

// --- ingest ---

func runIngest(args []string) error {
	fs := flag.NewFlagSet("ingest", flag.ExitOnError)
	cf := bindCommon(fs)
	repo := fs.String("repo", "", "override graph name (default: .git ancestor of paths[0])")
	strict := fs.Bool("strict", false, "only backtick-fenced identifiers create DOCUMENTS edges")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "usage: h9s-cli ingest <path>... [flags]\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		fs.Usage()
		return fmt.Errorf("at least one path is required")
	}
	paths := fs.Args()

	repoName, err := pipeline.ResolvePathsToRepo(*repo, paths)
	if err != nil {
		return fmt.Errorf("resolving repo: %w", err)
	}

	store, err := falkorstore.NewStore(*cf.falkorAddr, repoName)
	if err != nil {
		return fmt.Errorf("connecting to FalkorDB for graph %q: %w", repoName, err)
	}
	defer store.Close()
	if err := store.EnsureIndex(context.Background(), pipeline.EmbeddingDim); err != nil {
		return fmt.Errorf("ensuring chunk vector index: %w", err)
	}
	if err := store.EnsureCodeIndex(context.Background()); err != nil {
		return fmt.Errorf("ensuring code index: %w", err)
	}

	embedder := embedding.NewClient(*cf.embeddingURL, pipeline.EmbeddingDim)
	res, err := pipeline.IngestDocs(context.Background(), store, embedder, paths, nil, *strict)
	if err != nil {
		return err
	}
	if res.ChunksStored == 0 && res.OversizedChunks == 0 {
		fmt.Printf("No .md files found in the provided paths (graph %q).\n", repoName)
		return nil
	}
	fmt.Printf("Ingested %d chunks (%d oversized, no vector) into graph %q; %d DOCUMENTS edges asserted.\n",
		res.ChunksStored, res.OversizedChunks, repoName, res.EdgesAsserted)
	return nil
}

// --- search ---

func runSearch(args []string) error {
	fs := flag.NewFlagSet("search", flag.ExitOnError)
	cf := bindCommon(fs)
	k := fs.Int("k", 5, "number of results")
	repo := fs.String("repo", "", "graph to search (default: --graph)")
	path := fs.String("path", "", "path to resolve repo from via .git walk-up")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "usage: h9s-cli search <query> [flags]\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		fs.Usage()
		return fmt.Errorf("query is required")
	}
	query := strings.Join(fs.Args(), " ")

	graphName := *cf.graph
	if *repo != "" || *path != "" {
		name, err := pipeline.ResolveRepoGraphName(*repo, *path)
		if err != nil {
			return fmt.Errorf("resolving repo: %w", err)
		}
		graphName = name
	}

	store, err := falkorstore.NewStore(*cf.falkorAddr, graphName)
	if err != nil {
		return fmt.Errorf("connecting to FalkorDB for graph %q: %w", graphName, err)
	}
	defer store.Close()
	embedder := embedding.NewClient(*cf.embeddingURL, pipeline.EmbeddingDim)

	ctx := context.Background()
	vectors, err := embedder.Embed(ctx, []string{query})
	if err != nil {
		return fmt.Errorf("embedding query: %w", err)
	}
	results, err := store.Search(ctx, vectors[0], *k)
	if err != nil {
		return fmt.Errorf("searching graph %q: %w", graphName, err)
	}
	if len(results) == 0 {
		fmt.Println("No results.")
		return nil
	}
	for i, r := range results {
		fmt.Printf("### Result %d (score: %.4f)\n", i+1, r.Score)
		fmt.Printf("Source:    %s (lines %d–%d)\n", r.Source, r.LineStart, r.LineEnd)
		fmt.Printf("Breadcrumb: %s\n\n", r.Breadcrumb)
		content := r.Content
		if len(content) > 500 {
			content = content[:500] + "..."
		}
		fmt.Println(content)
		fmt.Println("\n---")
	}
	return nil
}

// --- link ---

func runLink(args []string) error {
	fs := flag.NewFlagSet("link", flag.ExitOnError)
	cf := bindCommon(fs)
	repo := fs.String("repo", "", "graph to link")
	path := fs.String("path", "", "path to resolve repo from via .git walk-up")
	strict := fs.Bool("strict", false, "only backtick-fenced identifiers create edges")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "usage: h9s-cli link [flags]\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	repoName, err := pipeline.ResolveRepoGraphName(*repo, *path)
	if err != nil {
		return fmt.Errorf("resolving repo: %w", err)
	}
	store, err := falkorstore.NewStore(*cf.falkorAddr, repoName)
	if err != nil {
		return fmt.Errorf("connecting to FalkorDB for graph %q: %w", repoName, err)
	}
	defer store.Close()

	edges, err := pipeline.LinkRepo(context.Background(), store, *strict)
	if err != nil {
		return err
	}
	fmt.Printf("Asserted %d DOCUMENTS edges in graph %q.\n", edges, repoName)
	return nil
}

// --- backfill-anchors ---

func runBackfillAnchors(args []string) error {
	fs := flag.NewFlagSet("backfill-anchors", flag.ExitOnError)
	cf := bindCommon(fs)
	repo := fs.String("repo", "", "graph to backfill (default: --graph)")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "usage: h9s-cli backfill-anchors [flags]\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	graphName := *cf.graph
	if *repo != "" {
		graphName = *repo
	}

	store, err := falkorstore.NewStore(*cf.falkorAddr, graphName)
	if err != nil {
		return fmt.Errorf("connecting to FalkorDB for graph %q: %w", graphName, err)
	}
	defer store.Close()

	count, err := store.BackfillChunkAnchors(context.Background())
	if err != nil {
		return err
	}
	fmt.Printf("Backfilled anchor_id/content_hash on %d chunk(s) in graph %q.\n", count, graphName)
	return nil
}

// --- helpers ---

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func lastSegment(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' || p[i] == '\\' {
			return p[i+1:]
		}
	}
	return p
}
