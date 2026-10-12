// Package llmfactory builds the llm.Client the process runs on, and refuses to start
// when the configuration would put the wrong model behind the product. It is a
// package of its own because adapters import llm, and llm cannot import them back.
package llmfactory

import (
	"fmt"
	"log/slog"
	"os"

	"kiwi/internal/platform/config"
	"kiwi/internal/platform/llm"
	"kiwi/internal/platform/llm/openai"
)

const (
	envProd        = "prod"
	providerOpenAI = "openai"
	providerVertex = "vertex"

	// maxRetries is how often a call the provider refused as busy is repeated.
	maxRetries = 3
)

// New returns the client for cfg.LLMProvider, wrapped so that calls the provider
// refuses as busy are retried.
//
// With KIWI_ENV=prod anything but vertex is an error, and it is the first thing
// checked: the local model is uncensored, and the submission must be served by
// Gemini, so no configuration of prod may reach it. vertex itself is not built in the
// local MVP, so a binary of this plan cannot serve prod at all.
func New(cfg config.Config) (llm.Client, error) {
	if cfg.Env == envProd && cfg.LLMProvider != providerVertex {
		return nil, fmt.Errorf("KIWI_ENV=prod requires KIWI_LLM_PROVIDER=%s, got %q: the local model must not serve production", providerVertex, cfg.LLMProvider)
	}
	switch cfg.LLMProvider {
	case providerOpenAI:
		c, err := openai.New(cfg, nil)
		if err != nil {
			return nil, err
		}
		return llm.WithRetry(c, llm.RetryOptions{MaxRetries: maxRetries}), nil
	case providerVertex:
		return nil, fmt.Errorf("KIWI_LLM_PROVIDER=%s is not built in the local MVP (only %s)", providerVertex, providerOpenAI)
	}
	return nil, fmt.Errorf("unknown KIWI_LLM_PROVIDER %q: want %s", cfg.LLMProvider, providerOpenAI)
}

// MustNew is New for a process that cannot run without its model: it logs the error
// and exits with status 1.
func MustNew(cfg config.Config) llm.Client {
	c, err := New(cfg)
	if err != nil {
		slog.Error("llm: refusing to start", "error", err)
		os.Exit(1)
	}
	return c
}
