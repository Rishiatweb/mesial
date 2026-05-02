.PHONY: up down embed ingest build cli smoke

# Start all services (FalkorDB, ingest, MCP Gateway)
up:
	docker compose up -d

down:
	docker compose down

# Start the embedding server (runs in foreground).
# --ctx-size 8192 handles long doc chunks (default is too small and crashes
# the server on inputs above a few hundred tokens). Qwen3 supports up to 32k.
embed:
	llama-server --embedding -m models/Qwen3-Embedding-0.6B-Q8_0.gguf --port 8090 --ctx-size 8192

# Run the ingest MCP server in HTTP mode (for dev/testing)
ingest:
	FALKOR_ADDR=localhost:6381 EMBEDDING_URL=http://localhost:8090 H9S_TRANSPORT=http go run ./cmd/ingest

# Build all Go binaries
build:
	go build ./...

# Build the dev CLI (h9s-cli) into the repo root.
cli:
	go build -o h9s-cli ./cmd/h9s-cli

# End-to-end smoke: run the CLI against a known repo. Requires FalkorDB
# (`make up`) and the embedding server (`make embed`) to be running.
# Override SMOKE_REPO to point at a different repository.
SMOKE_REPO ?= /Users/mknw/Code/kg-agent
smoke: cli
	./h9s-cli analyze $(SMOKE_REPO)
