package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/mknw/h9s/internal/chunking"
)

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
			found, err := chunking.FindMarkdown(arg)
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

	var allChunks []chunking.Chunk
	for _, f := range files {
		chunks, err := chunking.ChunkFile(f)
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
