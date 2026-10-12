package openai

import (
	"context"
	"fmt"

	"kiwi/internal/platform/llm"
)

const (
	// embedDim is the dimension of every vector: chunks.embedding is vector(768).
	embedDim = 768

	// EmbeddingGemma is trained with a prefix that says what a text is for; without
	// it, or with the wrong one, search quality drops. These are the ones of its model
	// card, and what the retrieval check of the spike used.
	documentPrefix = "title: none | text: "
	queryPrefix    = "task: search result | query: "
)

type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embedResponse struct {
	Data []struct {
		Index     *int      `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

// Embed implements llm.Client. Embeddings come from Ollama, not LiteLLM, which has no
// embeddings endpoint; the credential of LiteLLM is not sent there. Ollama cuts an
// input of more than 2048 tokens without an error, so callers keep their chunks small.
func (c *client) Embed(ctx context.Context, texts []string, task llm.EmbedTask) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	prefix := documentPrefix
	if task == llm.EmbedQuery {
		prefix = queryPrefix
	}
	in := make([]string, len(texts))
	for i, t := range texts {
		in[i] = prefix + t
	}

	var resp embedResponse
	if err := c.postJSON(ctx, c.embedBase+"/v1/embeddings", false, embedRequest{Model: c.embedModel, Input: in}, &resp); err != nil {
		return nil, err
	}
	if len(resp.Data) != len(texts) {
		return nil, fmt.Errorf("openai: %d vectors for %d texts", len(resp.Data), len(texts))
	}
	out := make([][]float32, len(texts))
	for pos, d := range resp.Data {
		i := pos
		if d.Index != nil {
			i = *d.Index
		}
		if i < 0 || i >= len(out) || out[i] != nil {
			return nil, fmt.Errorf("openai: the vector at position %d has index %d, which is out of range or repeated", pos, i)
		}
		if len(d.Embedding) != embedDim {
			return nil, fmt.Errorf("openai: %s returned a vector of %d dimensions, want %d", c.embedModel, len(d.Embedding), embedDim)
		}
		out[i] = d.Embedding
	}
	return out, nil
}
