.PHONY: up down embed ingest build cli smoke

# Start all services (FalkorDB, ingest, MCP Gateway)
up:
	docker compose up -d

down:
	docker compose down

# Start the embedding server (runs in foreground).
# --ctx-size 8192 handles long doc chunks (default is too small and crashes
# the server on inputs above a few hundred tokens). Qwen3 supports up to 32k.
#
# EMBED_MODEL defaults to the repo-local (gitignored) models/ dir — drop your
# own GGUF there, or override EMBED_MODEL to point anywhere else (e.g. a
# shared copy from another checkout on your machine).
EMBED_MODEL ?= models/Qwen3-Embedding-0.6B-Q8_0.gguf
embed:
	@test -f "$(EMBED_MODEL)" || { echo "model file missing: $(EMBED_MODEL) — download Qwen3-Embedding-0.6B-Q8_0.gguf into models/, or set EMBED_MODEL=/path/to/your.gguf"; exit 1; }
	llama-server --embedding -m $(EMBED_MODEL) --port 8090 --ctx-size 8192

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
