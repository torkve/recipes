// Package embed is a tiny HTTP client for a self-hosted text-embedding service
// (HuggingFace text-embeddings-inference: POST /embed {"inputs": [...]} ->
// [[...floats...]]). It applies the e5 query:/passage: prefixes and returns
// L2-normalized vectors suitable for cosine similarity via dot product.
//
// The client uses only the standard library (no new dependency), so the offline
// build is unaffected.
package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// maxInputRunes caps passage/query length sent to the embedder; the model has a
// token budget and TEI truncates too, but trimming client-side keeps payloads
// small. Recipes are short, so this rarely bites.
const maxInputRunes = 2000

// Client talks to one embedding endpoint serving one model.
type Client struct {
	url   string // base URL, e.g. http://embedder:80
	model string
	dim   int
	http  *http.Client
}

// New returns a client, or nil when url is empty (semantic search disabled).
// Callers must nil-check; do not wrap a nil *Client in an interface.
func New(url, model string, dim int) *Client {
	if strings.TrimSpace(url) == "" {
		return nil
	}
	return &Client{
		url:   strings.TrimRight(url, "/"),
		model: model,
		dim:   dim,
		http:  &http.Client{Timeout: 30 * time.Second},
	}
}

// Model returns the configured model id (tags stored vectors).
func (c *Client) Model() string { return c.model }

// Dim returns the expected embedding dimension.
func (c *Client) Dim() int { return c.dim }

// EmbedQuery embeds a single search query (e5 "query:" prefix).
func (c *Client) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	vecs, err := c.embed(ctx, []string{"query: " + truncate(text)})
	if err != nil {
		return nil, err
	}
	if len(vecs) != 1 {
		return nil, fmt.Errorf("embed: query returned %d vectors, want 1", len(vecs))
	}
	return vecs[0], nil
}

// EmbedPassages embeds recipe documents (e5 "passage:" prefix), one vector per
// input, in order.
func (c *Client) EmbedPassages(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	inputs := make([]string, len(texts))
	for i, t := range texts {
		inputs[i] = "passage: " + truncate(t)
	}
	return c.embed(ctx, inputs)
}

func truncate(s string) string {
	r := []rune(s)
	if len(r) > maxInputRunes {
		return string(r[:maxInputRunes])
	}
	return s
}

func (c *Client) embed(ctx context.Context, inputs []string) ([][]float32, error) {
	body, err := json.Marshal(map[string]any{
		"inputs":    inputs,
		"normalize": true,
		"truncate":  true,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url+"/embed", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embed: %s returned status %d", c.url, resp.StatusCode)
	}
	var vecs [][]float32
	if err := json.NewDecoder(resp.Body).Decode(&vecs); err != nil {
		return nil, fmt.Errorf("embed: decode response: %w", err)
	}
	if len(vecs) != len(inputs) {
		return nil, fmt.Errorf("embed: got %d vectors for %d inputs", len(vecs), len(inputs))
	}
	for i, v := range vecs {
		if len(v) != c.dim {
			return nil, fmt.Errorf("embed: vector %d has dim %d, want %d", i, len(v), c.dim)
		}
	}
	return vecs, nil
}
