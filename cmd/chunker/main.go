package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type Chunk struct {
	Breadcrumb string `json:"breadcrumb"`
	Content    string `json:"content"`
	Source     string `json:"source"`
	LineStart  int    `json:"line_start"`
	LineEnd    int    `json:"line_end"`
}

var headingRe = regexp.MustCompile(`^(#{1,6})\s+(.+)$`)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: chunker <path> [path...]\n")
		fmt.Fprintf(os.Stderr, "  path can be a .md file or a directory (scanned recursively)\n")
		os.Exit(1)
	}

	var files []string
	for _, arg := range os.Args[1:] {
		info, err := os.Stat(arg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %s: %v\n", arg, err)
			os.Exit(1)
		}
		if info.IsDir() {
			found, err := findMarkdown(arg)
			if err != nil {
				fmt.Fprintf(os.Stderr, "error scanning %s: %v\n", arg, err)
				os.Exit(1)
			}
			files = append(files, found...)
		} else {
			files = append(files, arg)
		}
	}

	if len(files) == 0 {
		fmt.Fprintf(os.Stderr, "no .md files found\n")
		os.Exit(1)
	}

	var allChunks []Chunk
	for _, f := range files {
		chunks, err := chunkFile(f)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error chunking %s: %v\n", f, err)
			os.Exit(1)
		}
		allChunks = append(allChunks, chunks...)
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(allChunks); err != nil {
		fmt.Fprintf(os.Stderr, "error encoding JSON: %v\n", err)
		os.Exit(1)
	}
}

func findMarkdown(root string) ([]string, error) {
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

func chunkFile(path string) ([]Chunk, error) {
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
		breadcrumb := buildBreadcrumb(headings)
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

func buildBreadcrumb(headings []string) string {
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
