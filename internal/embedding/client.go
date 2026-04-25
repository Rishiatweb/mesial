package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"
)

const defaultBatchSize = 32

// Client calls the llama-server /v1/embeddings endpoint.
type Client struct {
	baseURL    string
	httpClient *http.Client
	dim        int // if > 0, truncate vectors to this many dimensions (Matryoshka)
}

// NewClient creates an embedding client pointing at the given llama-server base URL.
// If dim > 0, vectors are truncated to dim dimensions (Matryoshka embedding support).
func NewClient(baseURL string, dim int) *Client {
	return &Client{
		baseURL: baseURL,
		dim:     dim,
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
}

type embeddingRequest struct {
	Input          any    `json:"input"`
	Model          string `json:"model"`
	EncodingFormat string `json:"encoding_format"`
}

type embeddingResponse struct {
	Data []embeddingData `json:"data"`
}

type embeddingData struct {
	Index     int       `json:"index"`
	Embedding []float64 `json:"embedding"`
}

// Embed sends texts to llama-server and returns float32 vectors.
// Texts are batched in groups of 32 to avoid overloading the server.
func (c *Client) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	results := make([][]float32, len(texts))

	for start := 0; start < len(texts); start += defaultBatchSize {
		end := start + defaultBatchSize
		if end > len(texts) {
			end = len(texts)
		}
		batch := texts[start:end]

		vectors, err := c.embedBatch(ctx, batch)
		if err != nil {
			return nil, fmt.Errorf("embedding batch [%d:%d]: %w", start, end, err)
		}

		for i, v := range vectors {
			results[start+i] = v
		}
	}

	return results, nil
}

func (c *Client) embedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	var input any
	if len(texts) == 1 {
		input = texts[0]
	} else {
		input = texts
	}

	reqBody := embeddingRequest{
		Input:          input,
		Model:          "any",
		EncodingFormat: "float",
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embedding request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("embedding server returned %d: %s", resp.StatusCode, string(respBody))
	}

	var embResp embeddingResponse
	if err := json.NewDecoder(resp.Body).Decode(&embResp); err != nil {
		return nil, fmt.Errorf("decoding embedding response: %w", err)
	}

	// Sort by index to ensure correct ordering
	sort.Slice(embResp.Data, func(i, j int) bool {
		return embResp.Data[i].Index < embResp.Data[j].Index
	})

	vectors := make([][]float32, len(embResp.Data))
	for i, d := range embResp.Data {
		dim := len(d.Embedding)
		if c.dim > 0 && c.dim < dim {
			dim = c.dim
		}
		vec := make([]float32, dim)
		for j := 0; j < dim; j++ {
			vec[j] = float32(d.Embedding[j])
		}
		vectors[i] = vec
	}

	if len(vectors) != len(texts) {
		return nil, fmt.Errorf("expected %d embeddings, got %d", len(texts), len(vectors))
	}

	return vectors, nil
}
