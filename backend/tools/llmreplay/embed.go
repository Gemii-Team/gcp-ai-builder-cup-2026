package main

import (
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// embedDim is the dimension of every vector: chunks.embedding is vector(768).
const embedDim = 768

// taskPrefixes are what the adapter puts in front of every text for EmbeddingGemma: an
// instruction for the model, not words of the text. They are left out of the hash, so
// the same words as a document and as a query are the same vector.
var taskPrefixes = []string{"title: none | text: ", "task: search result | query: "}

func stripTaskPrefix(s string) string {
	for _, p := range taskPrefixes {
		if rest, ok := strings.CutPrefix(s, p); ok {
			return rest
		}
	}
	return s
}

// hashedEmbedding is the vector of text: its character trigrams, over the NFKC form of
// the lower-cased text with whitespace collapsed and two spaces of padding on each
// side, hashed (FNV-1a) into embedDim signed counters, L2-normalised. Texts that share
// words share trigrams, so their cosine similarity is high; unrelated texts hash
// to unrelated counters, so theirs is near zero. Characters need no word boundaries,
// which Thai has none of. Any text has a vector: padding gives even "" some trigrams.
func hashedEmbedding(text string) []float32 {
	text = norm.NFKC.String(strings.ToValidUTF8(text, ""))
	text = strings.Join(strings.Fields(strings.ToLower(text)), " ")
	runes := []rune("  " + text + "  ")

	var counts [embedDim]float64
	var buf [3 * utf8.UTFMax]byte
	for i := 0; i+3 <= len(runes); i++ {
		b := buf[:0]
		for _, r := range runes[i : i+3] {
			b = utf8.AppendRune(b, r)
		}
		h := fnv1a(b)
		if h>>63 == 0 {
			counts[h%embedDim]++
		} else {
			counts[h%embedDim]--
		}
	}

	var sum float64
	for _, c := range counts {
		sum += c * c
	}
	if sum == 0 {
		// Every trigram cancelled out: a fixed unit vector keeps the answer usable.
		counts[fnv1a([]byte(text))%embedDim] = 1
		sum = 1
	}
	out := make([]float32, embedDim)
	scale := 1 / math.Sqrt(sum)
	for i, c := range counts {
		out[i] = float32(c * scale)
	}
	return out
}

func fnv1a(b []byte) uint64 {
	h := uint64(14695981039346656037)
	for _, c := range b {
		h ^= uint64(c)
		h *= 1099511628211
	}
	return h
}

// embeddings answers POST /v1/embeddings the way Ollama's OpenAI endpoint does: one
// 768-dimension vector per input, in order, for any text. It never needs a fixture.
func (rp *replay) embeddings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model string          `json:"model"`
		Input json.RawMessage `json:"input"`
	}
	if !rp.decode(w, r, &req) {
		return
	}
	var inputs []string
	var one string
	if err := json.Unmarshal(req.Input, &inputs); err != nil {
		if err := json.Unmarshal(req.Input, &one); err != nil {
			rp.fail(w, http.StatusBadRequest, "input must be a string or a list of strings")
			return
		}
		inputs = []string{one}
	}
	if len(inputs) == 0 {
		rp.fail(w, http.StatusBadRequest, "input is empty")
		return
	}
	type embedding struct {
		Object    string    `json:"object"`
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	}
	data := make([]embedding, len(inputs))
	for i, text := range inputs {
		data[i] = embedding{Object: "embedding", Index: i, Embedding: hashedEmbedding(stripTaskPrefix(text))}
	}
	rp.log.Info("replayed", "path", r.URL.Path, "inputs", len(inputs))
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list", "model": req.Model, "data": data,
		"usage": map[string]int{"prompt_tokens": 0, "total_tokens": 0},
	})
}
