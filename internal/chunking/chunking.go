package chunking

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Chunk represents a section of a markdown file split on heading boundaries.
type Chunk struct {
	Breadcrumb string `json:"breadcrumb" redis:"breadcrumb"`
	Content    string `json:"content"    redis:"content"`
	Source     string `json:"source"     redis:"source"`
	LineStart  int    `json:"line_start" redis:"line_start"`
	LineEnd    int    `json:"line_end"   redis:"line_end"`
}

var headingRe = regexp.MustCompile(`^(#{1,6})\s+(.+)$`)

// ChunkFile splits a single markdown file into chunks by heading boundaries.
func ChunkFile(path string) ([]Chunk, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	absPath, _ := filepath.Abs(path)

	// headings tracks the current heading at each level (index 0 = #, 1 = ##, etc.)
	headings := make([]string, 6)
	var chunks []Chunk
	var currentLines []string
	currentStart := 1

	scanner := bufio.NewScanner(f)
	lineNum := 0

	flush := func() {
		content := strings.TrimSpace(strings.Join(currentLines, "\n"))
		if content == "" {
			return
		}
		breadcrumb := BuildBreadcrumb(headings)
		chunks = append(chunks, Chunk{
			Breadcrumb: breadcrumb,
			Content:    content,
			Source:     absPath,
			LineStart:  currentStart,
			LineEnd:    lineNum,
		})
	}

	for scanner.Scan() {
		lineNum++
		line := scanner.Text()

		m := headingRe.FindStringSubmatch(line)
		if m != nil {
			// Flush previous chunk
			flush()

			level := len(m[1]) // number of # characters
			title := strings.TrimSpace(m[2])

			// Set this level's heading and clear deeper levels
			headings[level-1] = title
			for i := level; i < 6; i++ {
				headings[i] = ""
			}

			currentLines = nil
			currentStart = lineNum
			continue
		}

		currentLines = append(currentLines, line)
	}

	// Flush final chunk
	flush()

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return chunks, nil
}

// FindMarkdown recursively finds all .md files under root.
func FindMarkdown(root string) ([]string, error) {
	var files []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.HasSuffix(strings.ToLower(info.Name()), ".md") {
			files = append(files, path)
		}
		return nil
	})
	return files, err
}

// BuildBreadcrumb joins non-empty heading levels with " > ".
func BuildBreadcrumb(headings []string) string {
	var parts []string
	for _, h := range headings {
		if h != "" {
			parts = append(parts, h)
		}
	}
	if len(parts) == 0 {
		return "(top)"
	}
	return strings.Join(parts, " > ")
}
