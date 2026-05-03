package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/mknw/h9s/internal/embedding"
	"github.com/mknw/h9s/internal/falkorstore"
	"github.com/mknw/h9s/internal/pipeline"
)

// runMemory dispatches `h9s-cli memory <sub>` to the appropriate handler.
func runMemory(args []string) error {
	if len(args) < 1 {
		memoryUsage(os.Stderr)
		return fmt.Errorf("memory subcommand required")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "init":
		return runMemoryInit(rest)
	case "add-obs":
		return runMemoryAddObs(rest)
	case "create-fact":
		return runMemoryCreateFact(rest)
	case "link-evidence":
		return runMemoryLinkEvidence(rest)
	case "link-motivates":
		return runMemoryLinkMotivates(rest)
	case "search-obs":
		return runMemorySearchObs(rest)
	case "search-facts":
		return runMemorySearchFacts(rest)
	case "show-obs":
		return runMemoryShowObs(rest)
	case "show-fact":
		return runMemoryShowFact(rest)
	case "evidence":
		return runMemoryEvidence(rest)
	case "-h", "--help", "help":
		memoryUsage(os.Stdout)
		return nil
	default:
		memoryUsage(os.Stderr)
		return fmt.Errorf("unknown memory subcommand: %s", sub)
	}
}

func memoryUsage(w *os.File) {
	fmt.Fprint(w, `h9s-cli memory — manage the memory layer (Fact / Observation / GraphMeta)

Usage:
  h9s-cli memory <sub> [flags]

Subcommands:
  init             Initialize a graph as kind=memory (writes :GraphMeta singleton + indexes)
  add-obs          Add an :Observation; returns its ID + KNN-similar facts
  create-fact      MERGE a :Fact triplet, link to a backing :Observation (no orphan facts)
  link-evidence    MERGE :EVIDENCE_FOR edges from one observation to existing facts
  link-motivates   MERGE :MOTIVATES from an :Observation to a :Chunk
  search-obs       KNN over observations (returns hits with cosine distance)
  search-facts     Structural search over facts (subject/predicate/object filters)
  show-obs         Inspect a single :Observation by ID (full record)
  show-fact        Inspect a single :Fact by ID (full record)
  evidence         List the :Observations backing a given :Fact via :EVIDENCE_FOR

All subcommands accept the common flags (--falkor-addr, --embedding-url, --graph)
plus --repo to target a specific per-graph store.
`)
}

// repoFlag returns the resolved graph name for a memory subcommand. Falls back
// to the global --graph flag when --repo isn't given.
func memoryGraphName(repo, defaultGraph string) string {
	if repo != "" {
		return repo
	}
	return defaultGraph
}

// --- memory init ---

func runMemoryInit(args []string) error {
	fs := flag.NewFlagSet("memory init", flag.ExitOnError)
	cf := bindCommon(fs)
	repo := fs.String("repo", "", "graph to initialize as kind=memory (default: --graph)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	graphName := memoryGraphName(*repo, *cf.graph)

	store, err := falkorstore.NewStore(*cf.falkorAddr, graphName)
	if err != nil {
		return fmt.Errorf("connecting to FalkorDB for graph %q: %w", graphName, err)
	}
	defer store.Close()

	if err := pipeline.EnsureMemoryReady(context.Background(), store, pipeline.MemoryGraphKind); err != nil {
		return err
	}
	fmt.Printf("Initialized graph %q as kind=memory (strict=false).\n", graphName)
	return nil
}

// --- memory add-obs ---

func runMemoryAddObs(args []string) error {
	fs := flag.NewFlagSet("memory add-obs", flag.ExitOnError)
	cf := bindCommon(fs)
	repo := fs.String("repo", "", "graph to write into (default: --graph)")
	text := fs.String("text", "", "observation text (required)")
	k := fs.Int("k", 5, "number of nearest existing observations to consult for similar_facts")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*text) == "" {
		return fmt.Errorf("--text is required")
	}
	graphName := memoryGraphName(*repo, *cf.graph)

	store, err := falkorstore.NewStore(*cf.falkorAddr, graphName)
	if err != nil {
		return fmt.Errorf("connecting to FalkorDB for graph %q: %w", graphName, err)
	}
	defer store.Close()
	if err := pipeline.EnsureMemoryReady(context.Background(), store, pipeline.MemoryGraphKind); err != nil {
		return err
	}
	embedder := embedding.NewClient(*cf.embeddingURL, pipeline.EmbeddingDim)

	res, err := pipeline.AddObservation(context.Background(), store, embedder, *text, *k)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(res)
}

// --- memory create-fact ---

func runMemoryCreateFact(args []string) error {
	fs := flag.NewFlagSet("memory create-fact", flag.ExitOnError)
	cf := bindCommon(fs)
	repo := fs.String("repo", "", "graph to write into (default: --graph)")
	obs := fs.Int64("obs", 0, "ID of the backing :Observation (required)")
	subject := fs.String("subject", "", "fact subject (required)")
	predicate := fs.String("predicate", "", "fact predicate (required; kernel: is_a, subtype_of, part_of, equivalent_to, incompatible_with, causes, requires)")
	object := fs.String("object", "", "fact object (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *obs <= 0 {
		return fmt.Errorf("--obs is required")
	}
	if *subject == "" || *predicate == "" || *object == "" {
		return fmt.Errorf("--subject, --predicate, --object are all required")
	}
	graphName := memoryGraphName(*repo, *cf.graph)

	store, err := falkorstore.NewStore(*cf.falkorAddr, graphName)
	if err != nil {
		return fmt.Errorf("connecting to FalkorDB for graph %q: %w", graphName, err)
	}
	defer store.Close()

	factID, err := pipeline.CreateFactFromObservation(context.Background(), store, *obs, *subject, *predicate, *object)
	if err != nil {
		return err
	}
	if !falkorstore.MemoryPredicateKernel[*predicate] {
		fmt.Fprintf(os.Stderr, "note: predicate %q is open-set — accepted but inert to the future inference engine.\n", *predicate)
	}
	fmt.Printf("Fact %d: (%s, %s, %s) — backed by observation %d.\n", factID, *subject, *predicate, *object, *obs)
	return nil
}

// --- memory link-evidence ---

func runMemoryLinkEvidence(args []string) error {
	fs := flag.NewFlagSet("memory link-evidence", flag.ExitOnError)
	cf := bindCommon(fs)
	repo := fs.String("repo", "", "graph (default: --graph)")
	obs := fs.Int64("obs", 0, "observation ID (required)")
	factsCSV := fs.String("facts", "", "comma-separated fact IDs (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *obs <= 0 {
		return fmt.Errorf("--obs is required")
	}
	factIDs, err := parseInt64CSV(*factsCSV)
	if err != nil {
		return fmt.Errorf("parsing --facts: %w", err)
	}
	if len(factIDs) == 0 {
		return fmt.Errorf("--facts is required")
	}
	graphName := memoryGraphName(*repo, *cf.graph)

	store, err := falkorstore.NewStore(*cf.falkorAddr, graphName)
	if err != nil {
		return fmt.Errorf("connecting to FalkorDB for graph %q: %w", graphName, err)
	}
	defer store.Close()

	if err := pipeline.LinkObservationEvidence(context.Background(), store, *obs, factIDs); err != nil {
		return err
	}
	fmt.Printf("Linked observation %d to %d facts via :EVIDENCE_FOR.\n", *obs, len(factIDs))
	return nil
}

// --- memory link-motivates ---

func runMemoryLinkMotivates(args []string) error {
	fs := flag.NewFlagSet("memory link-motivates", flag.ExitOnError)
	cf := bindCommon(fs)
	repo := fs.String("repo", "", "graph (default: --graph)")
	chunk := fs.Int64("chunk", 0, "chunk ID (required)")
	obs := fs.Int64("obs", 0, "observation ID (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *chunk <= 0 || *obs <= 0 {
		return fmt.Errorf("--chunk and --obs are required")
	}
	graphName := memoryGraphName(*repo, *cf.graph)

	store, err := falkorstore.NewStore(*cf.falkorAddr, graphName)
	if err != nil {
		return fmt.Errorf("connecting to FalkorDB for graph %q: %w", graphName, err)
	}
	defer store.Close()

	if err := pipeline.LinkObservationMotivatesChunk(context.Background(), store, *obs, *chunk); err != nil {
		return err
	}
	fmt.Printf("Linked :Observation %d -[:MOTIVATES]-> :Chunk %d.\n", *obs, *chunk)
	return nil
}

// --- memory search-obs ---

func runMemorySearchObs(args []string) error {
	fs := flag.NewFlagSet("memory search-obs", flag.ExitOnError)
	cf := bindCommon(fs)
	repo := fs.String("repo", "", "graph (default: --graph)")
	k := fs.Int("k", 5, "number of results")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("query text is required")
	}
	query := strings.Join(fs.Args(), " ")
	graphName := memoryGraphName(*repo, *cf.graph)

	store, err := falkorstore.NewStore(*cf.falkorAddr, graphName)
	if err != nil {
		return fmt.Errorf("connecting to FalkorDB for graph %q: %w", graphName, err)
	}
	defer store.Close()
	embedder := embedding.NewClient(*cf.embeddingURL, pipeline.EmbeddingDim)

	hits, err := pipeline.SearchObservations(context.Background(), store, embedder, query, *k)
	if err != nil {
		return err
	}
	if len(hits) == 0 {
		fmt.Println("No matching observations.")
		return nil
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(hits)
}

// --- memory search-facts ---

func runMemorySearchFacts(args []string) error {
	fs := flag.NewFlagSet("memory search-facts", flag.ExitOnError)
	cf := bindCommon(fs)
	repo := fs.String("repo", "", "graph (default: --graph)")
	subject := fs.String("subject", "", "exact subject filter (empty = wildcard)")
	predicate := fs.String("predicate", "", "exact predicate filter (empty = wildcard)")
	object := fs.String("object", "", "exact object filter (empty = wildcard)")
	limit := fs.Int("limit", 50, "max rows")
	if err := fs.Parse(args); err != nil {
		return err
	}
	graphName := memoryGraphName(*repo, *cf.graph)

	store, err := falkorstore.NewStore(*cf.falkorAddr, graphName)
	if err != nil {
		return fmt.Errorf("connecting to FalkorDB for graph %q: %w", graphName, err)
	}
	defer store.Close()

	rows, err := pipeline.SearchFacts(context.Background(), store, *subject, *predicate, *object, *limit)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Println("No matching facts.")
		return nil
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(rows)
}

// --- memory show-obs ---

func runMemoryShowObs(args []string) error {
	fs := flag.NewFlagSet("memory show-obs", flag.ExitOnError)
	cf := bindCommon(fs)
	repo := fs.String("repo", "", "graph (default: --graph)")
	id := fs.Int64("id", 0, "observation ID (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id <= 0 {
		return fmt.Errorf("--id is required")
	}
	graphName := memoryGraphName(*repo, *cf.graph)

	store, err := falkorstore.NewStore(*cf.falkorAddr, graphName)
	if err != nil {
		return fmt.Errorf("connecting to FalkorDB for graph %q: %w", graphName, err)
	}
	defer store.Close()

	rec, err := store.GetObservation(context.Background(), *id)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(rec)
}

// --- memory show-fact ---

func runMemoryShowFact(args []string) error {
	fs := flag.NewFlagSet("memory show-fact", flag.ExitOnError)
	cf := bindCommon(fs)
	repo := fs.String("repo", "", "graph (default: --graph)")
	id := fs.Int64("id", 0, "fact ID (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id <= 0 {
		return fmt.Errorf("--id is required")
	}
	graphName := memoryGraphName(*repo, *cf.graph)

	store, err := falkorstore.NewStore(*cf.falkorAddr, graphName)
	if err != nil {
		return fmt.Errorf("connecting to FalkorDB for graph %q: %w", graphName, err)
	}
	defer store.Close()

	rec, err := store.GetFact(context.Background(), *id)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(rec)
}

// --- memory evidence ---

func runMemoryEvidence(args []string) error {
	fs := flag.NewFlagSet("memory evidence", flag.ExitOnError)
	cf := bindCommon(fs)
	repo := fs.String("repo", "", "graph (default: --graph)")
	fact := fs.Int64("fact", 0, "fact ID to trace evidence for (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *fact <= 0 {
		return fmt.Errorf("--fact is required")
	}
	graphName := memoryGraphName(*repo, *cf.graph)

	store, err := falkorstore.NewStore(*cf.falkorAddr, graphName)
	if err != nil {
		return fmt.Errorf("connecting to FalkorDB for graph %q: %w", graphName, err)
	}
	defer store.Close()

	obs, err := store.EvidenceForFact(context.Background(), *fact)
	if err != nil {
		return err
	}
	if len(obs) == 0 {
		// :Fact with no evidence violates the no-orphan-facts invariant; surface
		// it as an empty array (not "no results") so the caller can detect it.
		obs = []falkorstore.ObservationRecord{}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(obs)
}

// parseInt64CSV parses "1,2,3" into []int64. Empty entries are skipped.
func parseInt64CSV(s string) ([]int64, error) {
	var out []int64
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		n, err := strconv.ParseInt(p, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid integer %q: %w", p, err)
		}
		out = append(out, n)
	}
	return out, nil
}
