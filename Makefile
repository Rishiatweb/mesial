.PHONY: up down embed ingest build

# Start all services (FalkorDB, ingest, MCP Gateway)
up:
	docker compose up -d

down:
	docker compose down

# Start the embedding server (runs in foreground)
embed:
	llama-server --embedding -m models/Qwen3-Embedding-0.6B-Q8_0.gguf --port 8090

# Run the ingest MCP server in HTTP mode (for dev/testing)
ingest:
	FALKOR_ADDR=localhost:6381 EMBEDDING_URL=http://localhost:8090 H9S_TRANSPORT=http go run ./cmd/ingest

# Build all Go binaries
build:
	go build ./...
