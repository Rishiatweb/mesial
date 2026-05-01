// Package analyzer defines the language-agnostic interface and types for
// source code analysis. Language-specific implementations (TypeScript, etc.)
// implement the Analyzer interface.
package analyzer

// SymbolKey categorizes unresolved symbols for relationship mapping in pass 2.
type SymbolKey string

const (
	SymCall            SymbolKey = "call"
	SymBaseClass       SymbolKey = "base_class"
	SymImplement       SymbolKey = "implement_interface"
	SymExtendInterface SymbolKey = "extend_interface"
	SymReturnType      SymbolKey = "return_type"
	SymParameter       SymbolKey = "parameters"
)

// RelationForSymbol maps a symbol key to the FalkorDB relationship type.
func RelationForSymbol(key SymbolKey) string {
	switch key {
	case SymCall:
		return "CALLS"
	case SymBaseClass:
		return "EXTENDS"
	case SymImplement:
		return "IMPLEMENTS"
	case SymExtendInterface:
		return "EXTENDS"
	case SymReturnType:
		return "RETURNS"
	case SymParameter:
		return "PARAMETERS"
	default:
		return ""
	}
}

// Symbol is an unresolved reference collected during pass 1 (tree-sitter).
// It records the position in the source where the reference appears,
// so that pass 2 (LSP) can resolve it via textDocument/definition.
type Symbol struct {
	Key  SymbolKey
	Path string // file path
	Row  uint   // 0-based line
	Col  uint   // 0-based column
}

// EntityInfo holds data collected per entity during pass 1.
type EntityInfo struct {
	Label    string        // Graph node label: Function, Class, Method, Interface, Enum, Constructor
	Name     string        // Declared name
	Doc      string        // Leading comment / JSDoc
	Path     string        // File path
	SrcStart uint          // Start line (0-based)
	SrcEnd   uint          // End line (0-based)
	GraphID  int64         // Set after MERGE into FalkorDB
	Symbols  []Symbol      // Unresolved references for pass 2
	Children []*EntityInfo // Nested entities (e.g. methods inside a class)
}

// FileInfo holds a parsed file and its top-level entities.
type FileInfo struct {
	Path     string        // Absolute file path
	Name     string        // Filename (e.g. "store.go")
	Ext      string        // Extension (e.g. ".go")
	GraphID  int64         // File node ID in FalkorDB
	Entities []*EntityInfo // Top-level entities
}

// Analyzer is the language-agnostic interface that each language implements.
type Analyzer interface {
	// Extensions returns file extensions this analyzer handles (e.g. [".ts", ".tsx"]).
	Extensions() []string

	// ParseFile parses source bytes and returns a FileInfo with entities and unresolved symbols.
	ParseFile(path string, source []byte) (*FileInfo, error)

	// IsDependency returns true if the file path is inside a dependency directory.
	IsDependency(path string) bool

	// LSPLanguageID returns the LSP language identifier for the given file path.
	LSPLanguageID(path string) string

	// Close releases any resources held by the analyzer (e.g. tree-sitter parsers).
	Close()
}
