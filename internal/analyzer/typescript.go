package analyzer

import (
	"path/filepath"
	"strings"
	"unsafe"

	tree_sitter "github.com/tree-sitter/go-tree-sitter"
	tree_sitter_typescript "github.com/tree-sitter/tree-sitter-typescript/bindings/go"
)

// TypeScriptAnalyzer parses TypeScript and TSX files using tree-sitter.
type TypeScriptAnalyzer struct {
	tsParser  *tree_sitter.Parser
	tsxParser *tree_sitter.Parser
}

// NewTypeScriptAnalyzer creates an analyzer with parsers for both .ts and .tsx.
func NewTypeScriptAnalyzer() *TypeScriptAnalyzer {
	tsParser := tree_sitter.NewParser()
	tsParser.SetLanguage(tree_sitter.NewLanguage(unsafe.Pointer(tree_sitter_typescript.LanguageTypescript())))

	tsxParser := tree_sitter.NewParser()
	tsxParser.SetLanguage(tree_sitter.NewLanguage(unsafe.Pointer(tree_sitter_typescript.LanguageTSX())))

	return &TypeScriptAnalyzer{
		tsParser:  tsParser,
		tsxParser: tsxParser,
	}
}

func (a *TypeScriptAnalyzer) Extensions() []string {
	return []string{".ts", ".tsx"}
}

func (a *TypeScriptAnalyzer) IsDependency(path string) bool {
	parts := strings.Split(filepath.ToSlash(path), "/")
	for _, p := range parts {
		if p == "node_modules" {
			return true
		}
	}
	return false
}

func (a *TypeScriptAnalyzer) LSPLanguageID(path string) string {
	if strings.HasSuffix(path, ".tsx") {
		return "typescriptreact"
	}
	return "typescript"
}

func (a *TypeScriptAnalyzer) Close() {
	a.tsParser.Close()
	a.tsxParser.Close()
}

func (a *TypeScriptAnalyzer) ParseFile(path string, source []byte) (*FileInfo, error) {
	parser := a.tsParser
	if strings.HasSuffix(path, ".tsx") {
		parser = a.tsxParser
	}

	tree := parser.Parse(source, nil)
	defer tree.Close()

	file := &FileInfo{
		Path: path,
		Name: filepath.Base(path),
		Ext:  filepath.Ext(path),
	}

	root := tree.RootNode()
	file.Entities = a.extractEntities(root, source, path)
	return file, nil
}

// entityTypes are the tree-sitter node kinds we recognize as entities.
var entityTypes = map[string]bool{
	"function_declaration":    true,
	"class_declaration":       true,
	"method_definition":       true,
	"interface_declaration":   true,
	"type_alias_declaration":  true,
	"enum_declaration":        true,
}

// extractEntities walks the AST and collects entities at the current level,
// recursing into class/interface bodies for nested entities.
func (a *TypeScriptAnalyzer) extractEntities(node *tree_sitter.Node, source []byte, path string) []*EntityInfo {
	var entities []*EntityInfo

	for i := uint(0); i < node.ChildCount(); i++ {
		child := node.Child(i)
		if child == nil {
			continue
		}

		kind := child.Kind()

		if entityTypes[kind] {
			entity := a.nodeToEntity(child, source, path)
			if entity != nil {
				entities = append(entities, entity)
			}
			continue
		}

		// Arrow function in const/let declaration
		if kind == "lexical_declaration" {
			if ent := a.extractArrowFunction(child, source, path); ent != nil {
				entities = append(entities, ent)
				continue
			}
		}

		// Export statements wrap entities — look inside
		if kind == "export_statement" {
			inner := a.extractEntities(child, source, path)
			entities = append(entities, inner...)
		}
	}

	return entities
}

// nodeToEntity converts a recognized AST node into an EntityInfo.
func (a *TypeScriptAnalyzer) nodeToEntity(node *tree_sitter.Node, source []byte, path string) *EntityInfo {
	kind := node.Kind()
	label := a.labelForKind(kind)
	name := a.extractName(node, source)

	if name == "" {
		return nil
	}

	// Constructor is a method_definition with name "constructor"
	if kind == "method_definition" && name == "constructor" {
		label = "Constructor"
	}

	entity := &EntityInfo{
		Label:    label,
		Name:     name,
		Doc:      a.extractDoc(node, source),
		Path:     path,
		SrcStart: node.StartPosition().Row,
		SrcEnd:   node.EndPosition().Row,
	}

	// Extract symbols (unresolved references)
	a.addSymbols(entity, node, source, path)

	// Extract children for class and interface bodies
	if kind == "class_declaration" || kind == "interface_declaration" {
		body := node.ChildByFieldName("body")
		if body != nil {
			entity.Children = a.extractEntities(body, source, path)
		}
	}

	return entity
}

// extractArrowFunction checks if a lexical_declaration contains an arrow function
// and returns it as a Function entity.
func (a *TypeScriptAnalyzer) extractArrowFunction(node *tree_sitter.Node, source []byte, path string) *EntityInfo {
	for i := uint(0); i < node.ChildCount(); i++ {
		child := node.Child(i)
		if child == nil || child.Kind() != "variable_declarator" {
			continue
		}
		nameNode := child.ChildByFieldName("name")
		valueNode := child.ChildByFieldName("value")
		if nameNode == nil || valueNode == nil {
			continue
		}
		if valueNode.Kind() != "arrow_function" {
			continue
		}

		entity := &EntityInfo{
			Label:    "Function",
			Name:     nameNode.Utf8Text(source),
			Doc:      a.extractDoc(node, source), // doc on the lexical_declaration
			Path:     path,
			SrcStart: node.StartPosition().Row,
			SrcEnd:   node.EndPosition().Row,
		}
		a.addSymbols(entity, valueNode, source, path)
		return entity
	}
	return nil
}

func (a *TypeScriptAnalyzer) labelForKind(kind string) string {
	switch kind {
	case "function_declaration":
		return "Function"
	case "class_declaration":
		return "Class"
	case "method_definition":
		return "Method"
	case "interface_declaration":
		return "Interface"
	case "type_alias_declaration":
		return "Interface"
	case "enum_declaration":
		return "Enum"
	default:
		return ""
	}
}

// extractName gets the declared name from an entity node.
func (a *TypeScriptAnalyzer) extractName(node *tree_sitter.Node, source []byte) string {
	nameNode := node.ChildByFieldName("name")
	if nameNode != nil {
		return nameNode.Utf8Text(source)
	}
	return ""
}

// extractDoc looks for a preceding comment sibling (JSDoc or line comment).
func (a *TypeScriptAnalyzer) extractDoc(node *tree_sitter.Node, source []byte) string {
	prev := node.PrevSibling()
	if prev == nil {
		return ""
	}
	if prev.Kind() == "comment" {
		return prev.Utf8Text(source)
	}
	return ""
}

// addSymbols extracts unresolved references from an entity for pass 2 resolution.
func (a *TypeScriptAnalyzer) addSymbols(entity *EntityInfo, node *tree_sitter.Node, source []byte, path string) {
	kind := node.Kind()

	switch kind {
	case "class_declaration":
		a.extractHeritage(entity, node, source, path)
		a.extractCallsInBody(entity, node.ChildByFieldName("body"), source, path)
	case "method_definition":
		a.extractReturnType(entity, node, source, path)
		a.extractParameterTypes(entity, node, source, path)
		a.extractCallsInBody(entity, node.ChildByFieldName("body"), source, path)
	case "function_declaration", "arrow_function":
		a.extractReturnType(entity, node, source, path)
		a.extractParameterTypes(entity, node, source, path)
		a.extractCallsInBody(entity, node.ChildByFieldName("body"), source, path)
	case "interface_declaration":
		a.extractInterfaceHeritage(entity, node, source, path)
	}
}

// extractHeritage extracts extends/implements from a class declaration.
func (a *TypeScriptAnalyzer) extractHeritage(entity *EntityInfo, node *tree_sitter.Node, source []byte, path string) {
	for i := uint(0); i < node.ChildCount(); i++ {
		child := node.Child(i)
		if child == nil {
			continue
		}
		if child.Kind() != "class_heritage" {
			continue
		}
		for j := uint(0); j < child.ChildCount(); j++ {
			clause := child.Child(j)
			if clause == nil {
				continue
			}
			switch clause.Kind() {
			case "extends_clause":
				a.addTypeRefs(entity, clause, SymBaseClass, source, path)
			case "implements_clause":
				a.addTypeRefs(entity, clause, SymImplement, source, path)
			}
		}
	}
}

// extractInterfaceHeritage extracts extends from an interface declaration.
func (a *TypeScriptAnalyzer) extractInterfaceHeritage(entity *EntityInfo, node *tree_sitter.Node, source []byte, path string) {
	for i := uint(0); i < node.ChildCount(); i++ {
		child := node.Child(i)
		if child == nil || child.Kind() != "extends_type_clause" {
			continue
		}
		a.addTypeRefs(entity, child, SymExtendInterface, source, path)
	}
}

// addTypeRefs finds type identifiers inside a heritage clause and adds them as symbols.
func (a *TypeScriptAnalyzer) addTypeRefs(entity *EntityInfo, clause *tree_sitter.Node, key SymbolKey, source []byte, path string) {
	for i := uint(0); i < clause.ChildCount(); i++ {
		child := clause.Child(i)
		if child == nil {
			continue
		}
		switch child.Kind() {
		case "identifier", "type_identifier":
			entity.Symbols = append(entity.Symbols, Symbol{
				Key:  key,
				Path: path,
				Row:  child.StartPosition().Row,
				Col:  child.StartPosition().Column,
			})
		case "generic_type":
			nameNode := child.ChildByFieldName("name")
			if nameNode != nil {
				entity.Symbols = append(entity.Symbols, Symbol{
					Key:  key,
					Path: path,
					Row:  nameNode.StartPosition().Row,
					Col:  nameNode.StartPosition().Column,
				})
			}
		}
	}
}

// extractReturnType extracts the return type annotation from a function/method.
func (a *TypeScriptAnalyzer) extractReturnType(entity *EntityInfo, node *tree_sitter.Node, source []byte, path string) {
	// Return type annotation is a direct child of the function/method node
	for i := uint(0); i < node.ChildCount(); i++ {
		child := node.Child(i)
		if child == nil || child.Kind() != "type_annotation" {
			continue
		}
		a.extractTypeSymbol(entity, child, SymReturnType, source, path)
		break
	}
}

// extractParameterTypes extracts type annotations from function parameters.
func (a *TypeScriptAnalyzer) extractParameterTypes(entity *EntityInfo, node *tree_sitter.Node, source []byte, path string) {
	params := node.ChildByFieldName("parameters")
	if params == nil {
		return
	}
	for i := uint(0); i < params.ChildCount(); i++ {
		param := params.Child(i)
		if param == nil {
			continue
		}
		if param.Kind() != "required_parameter" && param.Kind() != "optional_parameter" {
			continue
		}
		for j := uint(0); j < param.ChildCount(); j++ {
			child := param.Child(j)
			if child != nil && child.Kind() == "type_annotation" {
				a.extractTypeSymbol(entity, child, SymParameter, source, path)
			}
		}
	}
}

// extractTypeSymbol finds the meaningful type identifier inside a type_annotation.
func (a *TypeScriptAnalyzer) extractTypeSymbol(entity *EntityInfo, typeAnnotation *tree_sitter.Node, key SymbolKey, source []byte, path string) {
	for i := uint(0); i < typeAnnotation.ChildCount(); i++ {
		child := typeAnnotation.Child(i)
		if child == nil {
			continue
		}
		switch child.Kind() {
		case "type_identifier":
			entity.Symbols = append(entity.Symbols, Symbol{
				Key:  key,
				Path: path,
				Row:  child.StartPosition().Row,
				Col:  child.StartPosition().Column,
			})
		case "generic_type":
			nameNode := child.ChildByFieldName("name")
			if nameNode != nil {
				entity.Symbols = append(entity.Symbols, Symbol{
					Key:  key,
					Path: path,
					Row:  nameNode.StartPosition().Row,
					Col:  nameNode.StartPosition().Column,
				})
			}
		}
		// Skip predefined_type (string, number, void, etc.) — not user-defined
	}
}

// extractCallsInBody walks a statement block to find call expressions.
func (a *TypeScriptAnalyzer) extractCallsInBody(entity *EntityInfo, body *tree_sitter.Node, source []byte, path string) {
	if body == nil {
		return
	}
	a.walkForCalls(entity, body, source, path)
}

// walkForCalls recursively walks the AST to find call_expression nodes,
// but does not descend into nested function/class/method declarations.
func (a *TypeScriptAnalyzer) walkForCalls(entity *EntityInfo, node *tree_sitter.Node, source []byte, path string) {
	for i := uint(0); i < node.ChildCount(); i++ {
		child := node.Child(i)
		if child == nil {
			continue
		}

		kind := child.Kind()

		// Don't descend into nested entities — they have their own symbol collection
		if entityTypes[kind] || kind == "arrow_function" {
			continue
		}

		if kind == "call_expression" {
			funcNode := child.ChildByFieldName("function")
			if funcNode != nil {
				pos := a.callPosition(funcNode)
				if pos != nil {
					entity.Symbols = append(entity.Symbols, Symbol{
						Key:  SymCall,
						Path: path,
						Row:  pos.Row,
						Col:  pos.Column,
					})
				}
			}
		}

		a.walkForCalls(entity, child, source, path)
	}
}

// callPosition extracts the position of the function identifier from a call expression.
func (a *TypeScriptAnalyzer) callPosition(funcNode *tree_sitter.Node) *tree_sitter.Point {
	switch funcNode.Kind() {
	case "identifier":
		p := funcNode.StartPosition()
		return &p
	case "member_expression":
		prop := funcNode.ChildByFieldName("property")
		if prop != nil {
			p := prop.StartPosition()
			return &p
		}
	}
	return nil
}
