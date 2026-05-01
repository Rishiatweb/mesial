package analyzer

import (
	"context"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/mknw/h9s/internal/falkorstore"
	"github.com/mknw/h9s/internal/lspclient"
)

// Result holds analysis statistics.
type Result struct {
	Files         int
	Entities      int
	Relationships int
	Errors        int
}

// Orchestrator runs the two-pass analysis pipeline.
type Orchestrator struct {
	analyzer Analyzer
	store    *falkorstore.Store
	lspCmd   string
	lspArgs  []string
}

// NewOrchestrator creates an orchestrator with the given analyzer, store, and LSP command.
func NewOrchestrator(a Analyzer, s *falkorstore.Store, lspCmd string, lspArgs []string) *Orchestrator {
	return &Orchestrator{
		analyzer: a,
		store:    s,
		lspCmd:   lspCmd,
		lspArgs:  lspArgs,
	}
}

// Analyze runs both passes on a repository directory.
func (o *Orchestrator) Analyze(ctx context.Context, repoPath string, ignore []string) (*Result, error) {
	result := &Result{}

	// Create indexes
	if err := o.store.EnsureCodeIndex(ctx); err != nil {
		return nil, fmt.Errorf("ensuring code index: %w", err)
	}

	// Discover files
	files, err := o.discoverFiles(repoPath, ignore)
	if err != nil {
		return nil, fmt.Errorf("discovering files: %w", err)
	}

	log.Printf("pass 1: parsing %d files", len(files))

	// Pass 1: tree-sitter parsing
	fileInfos, err := o.pass1(ctx, files, result)
	if err != nil {
		return result, fmt.Errorf("pass 1: %w", err)
	}

	log.Printf("pass 1 complete: %d files, %d entities", result.Files, result.Entities)

	// Pass 2: LSP symbol resolution
	if o.lspCmd != "" {
		log.Printf("pass 2: resolving symbols via %s", o.lspCmd)
		if err := o.pass2(ctx, repoPath, fileInfos, result); err != nil {
			// Pass 2 failure is non-fatal — pass 1 graph is still useful
			log.Printf("pass 2 warning: %v", err)
			result.Errors++
		} else {
			log.Printf("pass 2 complete: %d relationships", result.Relationships)
		}
	}

	return result, nil
}

// discoverFiles walks the repo and returns paths matching the analyzer's extensions.
func (o *Orchestrator) discoverFiles(repoPath string, ignore []string) ([]string, error) {
	extSet := make(map[string]bool)
	for _, ext := range o.analyzer.Extensions() {
		extSet[ext] = true
	}

	ignoreSet := make(map[string]bool)
	for _, dir := range ignore {
		ignoreSet[dir] = true
	}

	var files []string
	err := filepath.WalkDir(repoPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip errors
		}
		if d.IsDir() {
			if ignoreSet[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if extSet[filepath.Ext(path)] {
			files = append(files, path)
		}
		return nil
	})
	return files, err
}

// pass1 parses files with tree-sitter and creates nodes/DEFINES edges in FalkorDB.
func (o *Orchestrator) pass1(ctx context.Context, files []string, result *Result) (map[string]*FileInfo, error) {
	fileInfos := make(map[string]*FileInfo, len(files))

	for _, path := range files {
		source, err := os.ReadFile(path)
		if err != nil {
			log.Printf("skip %s: %v", path, err)
			result.Errors++
			continue
		}

		fileInfo, err := o.analyzer.ParseFile(path, source)
		if err != nil {
			log.Printf("skip %s: parse error: %v", path, err)
			result.Errors++
			continue
		}

		// Create File node
		fileID, err := o.store.AddFile(ctx, fileInfo.Path, fileInfo.Name, fileInfo.Ext)
		if err != nil {
			log.Printf("skip %s: store error: %v", path, err)
			result.Errors++
			continue
		}
		fileInfo.GraphID = fileID
		fileInfos[path] = fileInfo
		result.Files++

		// Create entity nodes and DEFINES edges
		for _, entity := range fileInfo.Entities {
			if err := o.storeEntity(ctx, entity, fileID, result); err != nil {
				log.Printf("entity error in %s: %v", path, err)
				result.Errors++
			}
		}
	}

	return fileInfos, nil
}

// storeEntity recursively stores an entity and its children, creating DEFINES edges.
func (o *Orchestrator) storeEntity(ctx context.Context, entity *EntityInfo, parentID int64, result *Result) error {
	entityID, err := o.store.AddEntity(ctx, entity.Label, entity.Name, entity.Doc, entity.Path, int(entity.SrcStart), int(entity.SrcEnd))
	if err != nil {
		return err
	}
	entity.GraphID = entityID
	result.Entities++

	// DEFINES edge from parent to this entity
	if err := o.store.ConnectEntities(ctx, "DEFINES", parentID, entityID, nil); err != nil {
		return fmt.Errorf("DEFINES edge: %w", err)
	}
	result.Relationships++

	// Recurse into children
	for _, child := range entity.Children {
		if err := o.storeEntity(ctx, child, entityID, result); err != nil {
			log.Printf("child entity error: %v", err)
			result.Errors++
		}
	}

	return nil
}

// pass2 starts the LSP server and resolves symbols to create relationship edges.
func (o *Orchestrator) pass2(ctx context.Context, repoPath string, fileInfos map[string]*FileInfo, result *Result) error {
	lsp, err := lspclient.Start(ctx, o.lspCmd, o.lspArgs, repoPath)
	if err != nil {
		return fmt.Errorf("starting LSP: %w", err)
	}
	defer lsp.Shutdown(ctx)

	// Open all files with the LSP server
	for path, fileInfo := range fileInfos {
		if o.analyzer.IsDependency(path) {
			continue
		}
		source, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		langID := o.analyzer.LSPLanguageID(path)
		if err := lsp.DidOpen(ctx, path, langID, string(source)); err != nil {
			log.Printf("LSP DidOpen %s: %v", path, err)
		}
		_ = fileInfo
	}

	// Resolve symbols for each file's entities
	for path, fileInfo := range fileInfos {
		if o.analyzer.IsDependency(path) {
			continue
		}
		o.resolveFileSymbols(ctx, lsp, fileInfo.Entities, fileInfos, result)
	}

	return nil
}

// resolveFileSymbols resolves symbols for a list of entities (recursive).
func (o *Orchestrator) resolveFileSymbols(ctx context.Context, lsp *lspclient.Client, entities []*EntityInfo, fileInfos map[string]*FileInfo, result *Result) {
	for _, entity := range entities {
		for _, sym := range entity.Symbols {
			o.resolveSymbol(ctx, lsp, entity, sym, fileInfos, result)
		}
		// Recurse into children
		o.resolveFileSymbols(ctx, lsp, entity.Children, fileInfos, result)
	}
}

// resolveSymbol uses LSP to resolve a single symbol reference and create a relationship edge.
func (o *Orchestrator) resolveSymbol(ctx context.Context, lsp *lspclient.Client, entity *EntityInfo, sym Symbol, fileInfos map[string]*FileInfo, result *Result) {
	locations, err := lsp.Definition(ctx, sym.Path, sym.Row, sym.Col)
	if err != nil || len(locations) == 0 {
		return
	}

	for _, loc := range locations {
		targetPath := lspclient.URIToPath(loc.URI)

		// Only resolve to files within our analyzed set
		if _, ok := fileInfos[targetPath]; !ok {
			continue
		}

		targetLine := int(loc.Range.Start.Line)
		targetID, err := o.store.LookupEntityByPosition(ctx, targetPath, targetLine)
		if err != nil || targetID < 0 {
			continue
		}

		relation := RelationForSymbol(sym.Key)
		if relation == "" {
			continue
		}

		var props map[string]interface{}
		if sym.Key == SymCall {
			props = map[string]interface{}{"pos": int(sym.Row)}
		}

		if err := o.store.ConnectEntities(ctx, relation, entity.GraphID, targetID, props); err != nil {
			log.Printf("connect %s: %v", relation, err)
			result.Errors++
			continue
		}
		result.Relationships++
	}
}

// AllEntities returns a flat list of all entities (including children) from all files.
func AllEntities(fileInfos map[string]*FileInfo) []*EntityInfo {
	var all []*EntityInfo
	for _, fi := range fileInfos {
		all = append(all, flattenEntities(fi.Entities)...)
	}
	return all
}

func flattenEntities(entities []*EntityInfo) []*EntityInfo {
	var flat []*EntityInfo
	for _, e := range entities {
		flat = append(flat, e)
		flat = append(flat, flattenEntities(e.Children)...)
	}
	return flat
}

// defaultIgnore is the default list of directories to skip during analysis.
var DefaultIgnore = []string{
	"node_modules", ".git", "dist", "build", "coverage", ".output",
	".next", "__pycache__", ".venv", "vendor",
}

// MergeIgnore combines user-provided ignore patterns with defaults.
func MergeIgnore(userIgnore []string) []string {
	if len(userIgnore) > 0 {
		return userIgnore
	}
	return DefaultIgnore
}

// RelativePath returns path relative to the repo root, or the original path.
func RelativePath(repoPath, filePath string) string {
	rel, err := filepath.Rel(repoPath, filePath)
	if err != nil {
		return filePath
	}
	return strings.TrimPrefix(rel, "./")
}
